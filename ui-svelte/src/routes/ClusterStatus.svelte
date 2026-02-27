<script lang="ts">
  import { onMount } from "svelte";
  import {
    applyClusterWizard,
    getClusterSettings,
    getClusterStatus,
    runClusterDGXUpdate,
    setClusterSettings,
  } from "../stores/api";
  import type { ClusterSettingsState, ClusterStatusState } from "../lib/types";
  import { collapseHomePath } from "../lib/pathDisplay";
  import {
    buildClusterVramSummary as buildVramSummary,
    connectivityProbeLabelForState as connectivityProbeLabel,
    connectivityStatusLabelForState as connectivityStatusLabel,
    formatCacheAgeMs as formatCacheAge,
    formatDurationMsLabel as formatDurationMs,
    formatMiB,
    formatNodeLatency as formatLatency,
    type NodeMetricSummary,
  } from "./clusterStatusViewModel";

  let loading = true;
  let refreshing = false;
  let metricsLoading = false;
  let storageLoading = false;
  let dgxLoading = false;
  let dgxUpdating = false;
  let dgxUpdatingTargets: Record<string, boolean> = {};
  let dgxUpdateConfirmKey = "";
  let error: string | null = null;
  let metricsError: string | null = null;
  let storageError: string | null = null;
  let dgxError: string | null = null;
  let dgxActionError: string | null = null;
  let dgxActionResult: string | null = null;
  let state: ClusterStatusState | null = null;
  let clusterSettings: ClusterSettingsState | null = null;
  let settingsLoading = false;
  let settingsSaving = false;
  let settingsError: string | null = null;
  let settingsResult: string | null = null;
  let settingsExecMode: "auto" | "local" | "agent" = "auto";
  let settingsInventoryFile = "";
  let wizardNodes = "";
  let wizardHeadNode = "";
  let wizardEthIf = "enp1s0f1np1";
  let wizardIbIf = "rocep1s0f1,roceP2p1s0f1";
  let wizardDefaultSSHUser = "csolutions_ai";
  let wizardInventoryFile = "";
  let wizardSaving = false;
  let requestGeneration = 0;
  const activeControllers = new Set<AbortController>();

  type ClusterDetailSection = "metrics" | "storage" | "dgx";
  type ClusterNode = ClusterStatusState["nodes"][number];

  function createRequestController(timeoutMs: number): AbortController {
    const controller = new AbortController();
    activeControllers.add(controller);
    const timeout = setTimeout(() => controller.abort(), timeoutMs);
    controller.signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timeout);
        activeControllers.delete(controller);
      },
      { once: true }
    );
    return controller;
  }

  function abortActiveRequests(): void {
    for (const controller of Array.from(activeControllers)) {
      controller.abort();
    }
    activeControllers.clear();
  }

  function formatTime(value?: string): string {
    if (!value) return "unknown";
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return value;
    return parsed.toLocaleString();
  }

  function formatProgress(progress?: number, status?: string): string {
    if (progress == null && !status) return "-";
    if (progress == null) return status || "-";
    if (!status) return `${progress}%`;
    return `${progress}% (${status})`;
  }

  function hasUpdatableDGXNodes(): boolean {
    if (!state) return false;
    return state.nodes.some((node) => isNodeDGXUpdatable(node));
  }

  function isNodeDGXUpdatable(node: ClusterStatusState["nodes"][number]): boolean {
    return (node.isLocal || node.sshOk) && node.dgx?.supported === true && node.dgx?.updateAvailable === true;
  }

  function isNodeUpdating(ip: string): boolean {
    return Boolean(dgxUpdatingTargets[ip]);
  }

  function markTargetsUpdating(targets: string[], updating: boolean): void {
    const next = { ...dgxUpdatingTargets };
    for (const target of targets) {
      if (!target) continue;
      if (updating) {
        next[target] = true;
      } else {
        delete next[target];
      }
    }
    dgxUpdatingTargets = next;
  }

  function storagePresence(ip: string, path: string) {
    return state?.storage?.nodes
      .find((n) => n.ip === ip)
      ?.paths.find((p) => p.path === path);
  }

  function setSectionsLoading(sections: ClusterDetailSection[], value: boolean): void {
    if (sections.includes("metrics")) metricsLoading = value;
    if (sections.includes("storage")) storageLoading = value;
    if (sections.includes("dgx")) dgxLoading = value;
  }

  function clearSectionErrors(sections: ClusterDetailSection[]): void {
    if (sections.includes("metrics")) metricsError = null;
    if (sections.includes("storage")) storageError = null;
    if (sections.includes("dgx")) dgxError = null;
  }

  function setSectionError(sections: ClusterDetailSection[], message: string): void {
    if (sections.includes("metrics")) metricsError = message;
    if (sections.includes("storage")) storageError = message;
    if (sections.includes("dgx")) dgxError = message;
  }

  function mergeState(next: ClusterStatusState, sections: ClusterDetailSection[]): void {
    if (!state) {
      state = next;
      return;
    }

    const includeSet = new Set(sections);
    const incomingByIP = new Map(next.nodes.map((node) => [node.ip, node]));
    const mergedNodes: ClusterNode[] = [];
    const seen = new Set<string>();
    for (const current of state.nodes) {
      const incoming = incomingByIP.get(current.ip);
      if (!incoming) {
        mergedNodes.push(current);
        continue;
      }
      seen.add(current.ip);
      const merged: ClusterNode = {
        ...current,
        id: incoming.id ?? current.id,
        controlIp: incoming.controlIp ?? current.controlIp,
        proxyIp: incoming.proxyIp ?? current.proxyIp,
        isLocal: incoming.isLocal,
        port22Open: incoming.port22Open,
        port22LatencyMs: incoming.port22LatencyMs ?? current.port22LatencyMs,
        port22Error: incoming.port22Error ?? current.port22Error,
        sshOk: incoming.sshOk,
        sshLatencyMs: incoming.sshLatencyMs ?? current.sshLatencyMs,
        sshError: incoming.sshError ?? current.sshError,
        error: incoming.error ?? current.error,
      };
      if (includeSet.has("metrics")) {
        merged.cpu = incoming.cpu;
        merged.disk = incoming.disk;
        merged.gpu = incoming.gpu;
      }
      if (includeSet.has("dgx")) {
        merged.dgx = incoming.dgx;
      }
      mergedNodes.push(merged);
    }
    for (const incoming of next.nodes) {
      if (seen.has(incoming.ip)) continue;
      mergedNodes.push(incoming);
    }

    state = {
      ...state,
      autodiscoverPath: next.autodiscoverPath,
      detectedAt: next.detectedAt,
      localIp: next.localIp,
      cidr: next.cidr,
      ethIf: next.ethIf,
      ibIf: next.ibIf,
      nodeCount: next.nodeCount,
      remoteCount: next.remoteCount,
      reachableBySsh: next.reachableBySsh,
      overall: next.overall,
      summary: next.summary,
      errors: next.errors,
      execMode: next.execMode ?? state.execMode,
      connectivityMode: next.connectivityMode ?? state.connectivityMode,
      cacheState: next.cacheState ?? state.cacheState,
      cacheAgeMs: next.cacheAgeMs ?? state.cacheAgeMs,
      timingsMs: next.timingsMs ?? state.timingsMs,
      nodes: mergedNodes,
      storage: includeSet.has("storage") ? next.storage : state.storage,
    };
  }

  function overallClass(overall: ClusterStatusState["overall"]): string {
    switch (overall) {
      case "healthy":
        return "border-green-400/40 bg-green-600/15 text-green-300";
      case "solo":
        return "border-sky-400/40 bg-sky-600/15 text-sky-300";
      case "degraded":
        return "border-amber-400/40 bg-amber-600/15 text-amber-300";
      default:
        return "border-error/40 bg-error/10 text-error";
    }
  }

  function clampPercent(value?: number | null): number {
    if (value == null || Number.isNaN(value)) return 0;
    if (value < 0) return 0;
    if (value > 100) return 100;
    return value;
  }

  function buildCpuSummary(node: ClusterStatusState["nodes"][number]): NodeMetricSummary {
    if (node.cpu?.error) {
      return { percent: null, label: "-", error: node.cpu.error };
    }
    const usage = node.cpu?.usagePercent;
    if (usage == null) {
      return { percent: null, label: "-" };
    }
    return { percent: usage, label: `${usage}%` };
  }

  function buildDiskSummary(node: ClusterStatusState["nodes"][number]): NodeMetricSummary {
    if (node.disk?.error) {
      return { percent: null, label: "-", error: node.disk.error };
    }
    const usage = node.disk?.usagePercent;
    const used = formatMiB(node.disk?.usedMiB);
    const total = formatMiB(node.disk?.totalMiB);
    if (usage == null) {
      return { percent: null, label: `${used} / ${total}` };
    }
    return { percent: usage, label: `${usage}% (${used} / ${total})` };
  }

  function buildGpuUtilSummary(node: ClusterStatusState["nodes"][number]): NodeMetricSummary {
    if (node.gpu?.error) {
      return { percent: null, label: "-", error: node.gpu.error };
    }
    const devices = node.gpu?.devices || [];
    if (devices.length === 0) {
      return { percent: null, label: "sin GPU" };
    }
    const utils = (node.gpu?.devices || [])
      .map((device) => device.utilizationPct)
      .filter((value): value is number => value != null);
    if (utils.length === 0) {
      return { percent: null, label: `N/A (${devices.length} GPU)` };
    }
    const avg = Math.round(utils.reduce((sum, current) => sum + current, 0) / utils.length);
    return { percent: avg, label: `${avg}% (${utils.length} GPU)` };
  }

  function applySettingsToForm(next: ClusterSettingsState): void {
    clusterSettings = next;
    settingsExecMode = (next.requestedExecMode || "auto") as "auto" | "local" | "agent";
    settingsInventoryFile = next.inventoryFile || "";
  }

  async function loadClusterSettingsState(): Promise<void> {
    settingsLoading = true;
    settingsError = null;
    try {
      const next = await getClusterSettings();
      applySettingsToForm(next);
    } catch (e) {
      settingsError = e instanceof Error ? e.message : String(e);
    } finally {
      settingsLoading = false;
    }
  }

  async function saveClusterSettings(): Promise<void> {
    settingsSaving = true;
    settingsError = null;
    settingsResult = null;
    try {
      const next = await setClusterSettings({
        execMode: settingsExecMode,
        inventoryFile: settingsInventoryFile.trim(),
      });
      applySettingsToForm(next);
      settingsResult = `Cluster mode actualizado: ${next.execMode}`;
      await refresh(true);
    } catch (e) {
      settingsError = e instanceof Error ? e.message : String(e);
    } finally {
      settingsSaving = false;
    }
  }

  async function runWizardClusterSettings(): Promise<void> {
    wizardSaving = true;
    settingsError = null;
    settingsResult = null;
    try {
      const next = await applyClusterWizard({
        nodes: wizardNodes,
        headNode: wizardHeadNode.trim(),
        ethIf: wizardEthIf.trim(),
        ibIf: wizardIbIf.trim(),
        defaultSshUser: wizardDefaultSSHUser.trim(),
        inventoryFile: wizardInventoryFile.trim(),
      });
      if (next.settings) {
        applySettingsToForm(next.settings);
      }
      const wizardInfo = next.wizard;
      settingsResult = wizardInfo?.inventoryFile
        ? `Wizard aplicado. Inventory guardado en ${wizardInfo.inventoryFile}`
        : "Wizard aplicado";
      await refresh(true);
    } catch (e) {
      settingsError = e instanceof Error ? e.message : String(e);
    } finally {
      wizardSaving = false;
    }
  }

  async function loadDetails(
    sections: ClusterDetailSection[],
    generation: number,
    forceRefresh: boolean
  ): Promise<void> {
    const controller = createRequestController(forceRefresh ? 45000 : 30000);
    try {
      const next = await getClusterStatus({
        signal: controller.signal,
        forceRefresh,
        view: "full",
        include: sections,
        allowStale: !forceRefresh,
      });
      if (generation !== requestGeneration) return;
      mergeState(next, sections);
      clearSectionErrors(sections);
    } catch (e) {
      if (generation !== requestGeneration) return;
      if (controller.signal.aborted) return;
      setSectionError(sections, e instanceof Error ? e.message : String(e));
    } finally {
      if (generation === requestGeneration) {
        setSectionsLoading(sections, false);
      }
    }
  }

  async function refresh(forceRefresh = false): Promise<void> {
    abortActiveRequests();
    const generation = ++requestGeneration;

    refreshing = true;
    error = null;
    metricsError = null;
    storageError = null;
    dgxError = null;
    if (!state) loading = true;
    const controller = createRequestController(15000);
    try {
      const summary = await getClusterStatus({
        signal: controller.signal,
        forceRefresh,
        view: "summary",
      });
      if (generation !== requestGeneration) return;
      mergeState(summary, []);
      if (!wizardNodes && summary.nodes && summary.nodes.length > 0) {
        wizardNodes = summary.nodes.map((node) => node.ip).join("\n");
      }
      setSectionsLoading(["metrics", "storage", "dgx"], true);
      void loadDetails(["metrics", "storage"], generation, forceRefresh);
      void loadDetails(["dgx"], generation, forceRefresh);
    } catch (e) {
      if (generation !== requestGeneration) return;
      if (controller.signal.aborted) {
        error = "Timeout al consultar el estado del cluster. Pulsa Refresh para reintentar.";
      } else {
        error = e instanceof Error ? e.message : String(e);
      }
      setSectionsLoading(["metrics", "storage", "dgx"], false);
    } finally {
      if (generation === requestGeneration) {
        refreshing = false;
        loading = false;
      }
    }
  }

  async function executeDgxUpdate(targets: string[]): Promise<void> {
    if (!state || dgxUpdating) return;
    if (targets.length === 0) {
      dgxActionError = "No hay nodos alcanzables por SSH para ejecutar UpdateAndReboot.";
      dgxActionResult = null;
      return;
    }

    dgxUpdating = true;
    dgxUpdateConfirmKey = "";
    markTargetsUpdating(targets, true);
    dgxActionError = null;
    dgxActionResult = null;
    try {
      const result = await runClusterDGXUpdate(targets);
      const lines = result.results.map((r) => `${r.ip}: ${r.ok ? "OK" : `FAIL (${r.error || "unknown"})`}`);
      dgxActionResult = `DGX update lanzado. OK=${result.success}, FAIL=${result.failed}\n${lines.join("\n")}`;
      await refresh(true);
    } catch (e) {
      dgxActionError = e instanceof Error ? e.message : String(e);
      dgxActionResult = null;
    } finally {
      markTargetsUpdating(targets, false);
      dgxUpdating = false;
    }
  }

  async function runDgxUpdate(): Promise<void> {
    if (!state || dgxUpdating) return;

    const targets = state.nodes.filter((node) => isNodeDGXUpdatable(node)).map((node) => node.ip);
    if (targets.length === 0) {
      dgxActionError = "No hay nodos alcanzables por SSH para ejecutar UpdateAndReboot.";
      dgxActionResult = null;
      return;
    }
    const confirmKey = targets.join(",");
    if (dgxUpdateConfirmKey !== confirmKey) {
      dgxUpdateConfirmKey = confirmKey;
      dgxActionError = null;
      dgxActionResult = "Confirma actualizacion: pulsa Update Nodes de nuevo para:\n" + targets.join("\n");
      return;
    }

    dgxUpdateConfirmKey = "";

    await executeDgxUpdate(targets);
  }

  async function runDgxUpdateNode(nodeIP: string): Promise<void> {
    if (!state || dgxUpdating) return;
    const node = state.nodes.find((n) => n.ip === nodeIP);
    if (!node || !isNodeDGXUpdatable(node)) {
      dgxActionError = `El nodo ${nodeIP} no está disponible para UpdateAndReboot.`;
      dgxActionResult = null;
      return;
    }

    await executeDgxUpdate([nodeIP]);
  }

  onMount(() => {
    void loadClusterSettingsState();
    void refresh(false);
    return () => {
      abortActiveRequests();
    };
  });
</script>

<div class="h-full flex flex-col gap-2">
  <div class="card shrink-0">
    <div class="flex items-center justify-between gap-2">
      <h2 class="pb-0">Cluster</h2>
      <div class="flex items-center gap-2">
        <button class="btn btn--sm" onclick={runDgxUpdate} disabled={dgxUpdating || !state || !hasUpdatableDGXNodes()}>
          {dgxUpdating ? "Updating..." : (dgxUpdateConfirmKey ? "Confirm Update Nodes" : "Update Nodes")}
        </button>
        <button class="btn btn--sm" onclick={() => refresh(true)} disabled={refreshing}>
          {refreshing ? "Refreshing..." : "Refresh"}
        </button>
      </div>
    </div>

    <div class="mt-3 rounded border border-card-border p-3 bg-background/40">
      <div class="text-sm font-semibold text-txtmain">Cluster Connectivity Config</div>
      <div class="mt-2 grid grid-cols-1 md:grid-cols-3 gap-2">
        <label class="text-xs text-txtsecondary">
          Mode
          <select class="input mt-1 w-full" bind:value={settingsExecMode} disabled={settingsSaving || settingsLoading}>
            <option value="auto">auto (inventory => agent)</option>
            <option value="local">local (autodiscover + ssh)</option>
            <option value="agent">agent (inventory + agent API)</option>
          </select>
        </label>
        <label class="text-xs text-txtsecondary md:col-span-2">
          Inventory File
          <input
            class="input mt-1 w-full font-mono text-xs"
            bind:value={settingsInventoryFile}
            placeholder="/path/to/cluster-inventory.yaml"
            disabled={settingsSaving || settingsLoading}
          />
        </label>
      </div>
      <div class="mt-2 flex items-center gap-2">
        <button class="btn btn--sm" onclick={saveClusterSettings} disabled={settingsSaving || settingsLoading}>
          {settingsSaving ? "Saving..." : "Save Cluster Config"}
        </button>
        {#if clusterSettings}
          <span class="text-xs text-txtsecondary">
            Effective mode: <span class="font-mono">{clusterSettings.execMode}</span>
            {#if clusterSettings.inventoryFile}
              · inventory: <span class="font-mono">{collapseHomePath(clusterSettings.inventoryFile)}</span>
            {/if}
          </span>
        {/if}
      </div>
      <div class="mt-3 border-t border-card-border pt-3">
        <div class="text-xs text-txtsecondary mb-2">Wizard rápido para generar inventory y activar mode=agent</div>
        <div class="grid grid-cols-1 md:grid-cols-3 gap-2">
          <label class="text-xs text-txtsecondary md:col-span-2">
            Nodes (IP/hostname por línea o coma)
            <textarea class="input mt-1 w-full min-h-[90px] font-mono text-xs" bind:value={wizardNodes}></textarea>
          </label>
          <div class="grid grid-cols-1 gap-2">
            <label class="text-xs text-txtsecondary">
              Head Node
              <input class="input mt-1 w-full font-mono text-xs" bind:value={wizardHeadNode} placeholder="192.168.8.121" />
            </label>
            <label class="text-xs text-txtsecondary">
              Default SSH User
              <input class="input mt-1 w-full font-mono text-xs" bind:value={wizardDefaultSSHUser} placeholder="csolutions_ai" />
            </label>
          </div>
          <label class="text-xs text-txtsecondary">
            RDMA ETH IF
            <input class="input mt-1 w-full font-mono text-xs" bind:value={wizardEthIf} />
          </label>
          <label class="text-xs text-txtsecondary">
            RDMA IB IF (comma separated)
            <input class="input mt-1 w-full font-mono text-xs" bind:value={wizardIbIf} />
          </label>
          <label class="text-xs text-txtsecondary">
            Inventory Output (optional)
            <input class="input mt-1 w-full font-mono text-xs" bind:value={wizardInventoryFile} placeholder="cluster-inventory.yaml" />
          </label>
        </div>
        <div class="mt-2">
          <button class="btn btn--sm" onclick={runWizardClusterSettings} disabled={wizardSaving || settingsSaving || settingsLoading}>
            {wizardSaving ? "Applying Wizard..." : "Apply Wizard"}
          </button>
        </div>
      </div>
      {#if settingsError}
        <div class="mt-2 text-xs text-error break-words">{settingsError}</div>
      {/if}
      {#if settingsResult}
        <div class="mt-2 text-xs text-green-300 break-words">{settingsResult}</div>
      {/if}
    </div>

    {#if state}
      <div class="mt-2 inline-flex items-center rounded border px-2 py-1 text-sm {overallClass(state.overall)}">
        {state.overall.toUpperCase()}
      </div>
      <div class="mt-2 text-sm text-txtsecondary">{state.summary}</div>
      <div class="text-xs text-txtsecondary break-all">
        autodiscover.sh:
        <span class="font-mono" title={state.autodiscoverPath}>{collapseHomePath(state.autodiscoverPath)}</span>
      </div>
      <div class="text-xs text-txtsecondary">
        Última comprobación: {formatTime(state.detectedAt)}
      </div>
      <div class="text-xs text-txtsecondary">
        Modo: {state.execMode || "-"} · Conectividad: {state.connectivityMode || "-"} · Cache: {state.cacheState || "-"} (
        {formatCacheAge(state.cacheAgeMs)}) · Total: {formatDurationMs(state.timingsMs?.total)}
      </div>
      {#if metricsLoading || storageLoading || dgxLoading}
        <div class="text-xs text-txtsecondary mt-1">
          Actualizando detalles:
          {metricsLoading ? " métricas" : ""}{storageLoading ? " storage" : ""}{dgxLoading ? " dgx" : ""}
        </div>
      {/if}
    {/if}

    {#if error}
      <div class="mt-2 p-2 border border-error/40 bg-error/10 rounded text-sm text-error break-words">{error}</div>
    {/if}
    {#if dgxActionError}
      <div class="mt-2 p-2 border border-error/40 bg-error/10 rounded text-sm text-error whitespace-pre-wrap break-words">
        {dgxActionError}
      </div>
    {/if}
    {#if dgxActionResult}
      <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-200 whitespace-pre-wrap break-words">
        {dgxActionResult}
      </div>
    {/if}
    {#if metricsError}
      <div class="mt-2 p-2 border border-amber-400/30 bg-amber-600/10 rounded text-sm text-amber-200 break-words">
        Error cargando métricas: {metricsError}
      </div>
    {/if}
    {#if storageError}
      <div class="mt-2 p-2 border border-amber-400/30 bg-amber-600/10 rounded text-sm text-amber-200 break-words">
        Error cargando storage: {storageError}
      </div>
    {/if}
    {#if dgxError}
      <div class="mt-2 p-2 border border-amber-400/30 bg-amber-600/10 rounded text-sm text-amber-200 break-words">
        Error cargando estado DGX: {dgxError}
      </div>
    {/if}
  </div>

  <div class="card flex-1 min-h-0 overflow-auto">
    {#if loading}
      <div class="text-sm text-txtsecondary">Comprobando conectividad del cluster...</div>
    {:else if state}
      <div class="grid grid-cols-1 md:grid-cols-3 gap-2 text-sm mb-3">
        <div class="rounded border border-card-border p-2">
          <div class="text-txtsecondary text-xs uppercase">Local IP</div>
          <div class="font-mono break-all">{state.localIp || "-"}</div>
          <div class="text-xs text-txtsecondary mt-1">CIDR: {state.cidr || "-"}</div>
        </div>
        <div class="rounded border border-card-border p-2">
          <div class="text-txtsecondary text-xs uppercase">Interfaces</div>
          <div class="font-mono break-all">ETH: {state.ethIf || "-"}</div>
          <div class="font-mono break-all">IB: {state.ibIf || "-"}</div>
        </div>
        <div class="rounded border border-card-border p-2">
          <div class="text-txtsecondary text-xs uppercase">Nodos</div>
          <div>Total: {state.nodeCount}</div>
          <div>Remotos: {state.remoteCount}</div>
          <div>{connectivityStatusLabel(state)}: {state.reachableBySsh}</div>
        </div>
      </div>

      <div class="mb-3">
        <div class="text-sm font-semibold text-txtmain">Recursos por nodo</div>
        {#if metricsLoading}
          <div class="text-xs text-txtsecondary mt-1">Cargando métricas (CPU, disco, GPU)...</div>
        {/if}
        <div class="mt-2 grid grid-cols-1 xl:grid-cols-2 gap-2">
          {#each state.nodes as node}
            {@const cpu = buildCpuSummary(node)}
            {@const disk = buildDiskSummary(node)}
            {@const gpuUtil = buildGpuUtilSummary(node)}
            {@const vram = buildVramSummary(node)}
            <div class="rounded border border-card-border p-3 bg-background/40">
              <div class="flex items-center justify-between gap-2">
                <div class="font-mono text-sm">{node.ip}</div>
                <div class="text-xs text-txtsecondary">{node.isLocal ? "local" : "remote"}</div>
              </div>
              <div class="mt-3 space-y-3">
                <div class="text-xs">
                  <div class="flex items-center justify-between gap-2">
                    <span class="text-txtsecondary uppercase">CPU</span>
                    <span class="text-txtmain">{cpu.label}</span>
                  </div>
                  {#if cpu.error}
                    <div class="text-error mt-1">{cpu.error}</div>
                  {:else}
                    <div class="mt-1 h-2 rounded bg-surface border border-card-border overflow-hidden">
                      <div class="h-full bg-gradient-to-r from-cyan-500 to-sky-400" style={`width: ${clampPercent(cpu.percent)}%`}></div>
                    </div>
                  {/if}
                </div>
                <div class="text-xs">
                  <div class="flex items-center justify-between gap-2">
                    <span class="text-txtsecondary uppercase">DISCO /</span>
                    <span class="text-txtmain">{disk.label}</span>
                  </div>
                  {#if disk.error}
                    <div class="text-error mt-1">{disk.error}</div>
                  {:else}
                    <div class="mt-1 h-2 rounded bg-surface border border-card-border overflow-hidden">
                      <div class="h-full bg-gradient-to-r from-emerald-500 to-lime-400" style={`width: ${clampPercent(disk.percent)}%`}></div>
                    </div>
                  {/if}
                </div>
                <div class="text-xs">
                  <div class="flex items-center justify-between gap-2">
                    <span class="text-txtsecondary uppercase">GPU UTIL</span>
                    <span class="text-txtmain">{gpuUtil.label}</span>
                  </div>
                  {#if gpuUtil.error}
                    <div class="text-error mt-1">{gpuUtil.error}</div>
                  {:else}
                    <div class="mt-1 h-2 rounded bg-surface border border-card-border overflow-hidden">
                      <div class="h-full bg-gradient-to-r from-violet-500 to-fuchsia-400" style={`width: ${clampPercent(gpuUtil.percent)}%`}></div>
                    </div>
                  {/if}
                </div>
                <div class="text-xs">
                  <div class="flex items-center justify-between gap-2">
                    <span class="text-txtsecondary uppercase">VRAM</span>
                    <span class="text-txtmain">{vram.label}</span>
                  </div>
                  {#if vram.error}
                    <div class="text-error mt-1">{vram.error}</div>
                  {:else}
                    <div class="mt-1 h-2 rounded bg-surface border border-card-border overflow-hidden">
                      <div class="h-full bg-gradient-to-r from-amber-500 to-orange-400" style={`width: ${clampPercent(vram.percent)}%`}></div>
                    </div>
                    {#if vram.note}
                      <div class="text-txtsecondary mt-1">{vram.note}</div>
                    {/if}
                  {/if}
                </div>
              </div>
            </div>
          {/each}
        </div>
      </div>

      {#if state.errors && state.errors.length > 0}
        <div class="mb-3 p-2 border border-amber-400/30 bg-amber-600/10 rounded">
          <div class="text-sm text-amber-300 font-semibold">Avisos de autodetección</div>
          <ul class="mt-1 text-sm text-amber-200 list-disc pl-5">
            {#each state.errors as line}
              <li>{line}</li>
            {/each}
          </ul>
        </div>
      {/if}

      {#if state.storage}
        <div class="mb-3 p-2 border border-card-border rounded bg-background/40">
          <div class="text-sm font-semibold text-txtmain">Almacenamiento Actual (baseline)</div>
          <div class="mt-1 text-xs text-txtsecondary">{state.storage.note}</div>
          {#if state.storage.duplicatePaths && state.storage.duplicatePaths.length > 0}
            <div class="mt-2 text-xs text-amber-300">
              Rutas presentes en varios nodos:
              {state.storage.duplicatePaths.map((p) => collapseHomePath(p)).join(", ")}
            </div>
          {/if}
          {#if state.storage.sharedAllPaths && state.storage.sharedAllPaths.length > 0}
            <div class="mt-1 text-xs text-sky-300">
              Rutas presentes en todos los nodos alcanzables:
              {state.storage.sharedAllPaths.map((p) => collapseHomePath(p)).join(", ")}
            </div>
          {/if}

          <div class="mt-2 overflow-auto border border-card-border rounded">
            <table class="w-full text-xs">
              <thead class="bg-surface">
                <tr>
                  <th class="text-left p-2 border-b border-card-border">Ruta</th>
                  {#each state.storage.nodes as n}
                    <th class="text-left p-2 border-b border-card-border">{n.ip}</th>
                  {/each}
                </tr>
              </thead>
              <tbody>
                {#each state.storage.paths as path}
                  <tr>
                    <td class="p-2 border-b border-card-border font-mono" title={path}>{collapseHomePath(path)}</td>
                    {#each state.storage.nodes as n}
                      {@const presence = storagePresence(n.ip, path)}
                      <td class="p-2 border-b border-card-border">
                        {#if presence?.error}
                          <span class="text-error">err</span>
                        {:else if presence?.exists}
                          <span class="text-green-300">present</span>
                        {:else}
                          <span class="text-txtsecondary">-</span>
                        {/if}
                      </td>
                    {/each}
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      {:else if storageLoading}
        <div class="mb-3 p-2 border border-card-border rounded bg-background/40 text-xs text-txtsecondary">
          Cargando baseline de almacenamiento...
        </div>
      {/if}

      <div class="overflow-auto border border-card-border rounded">
        <table class="w-full text-sm">
          <thead class="bg-surface">
            <tr>
              <th class="text-left p-2 border-b border-card-border">Nodo</th>
              <th class="text-left p-2 border-b border-card-border">Rol</th>
              <th class="text-left p-2 border-b border-card-border">Port 22</th>
              <th class="text-left p-2 border-b border-card-border">{connectivityProbeLabel(state)}</th>
              <th class="text-left p-2 border-b border-card-border">DGX Update</th>
              <th class="text-left p-2 border-b border-card-border">Acción</th>
              <th class="text-left p-2 border-b border-card-border">DGX Estado</th>
              <th class="text-left p-2 border-b border-card-border">Error</th>
            </tr>
          </thead>
          <tbody>
            {#each state.nodes as node}
              <tr>
                <td class="p-2 border-b border-card-border font-mono">{node.ip}</td>
                <td class="p-2 border-b border-card-border">{node.isLocal ? "local" : "remote"}</td>
                <td class="p-2 border-b border-card-border">
                  <span class={node.port22Open ? "text-green-300" : "text-error"}>
                    {node.port22Open ? "OK" : "FAIL"}
                  </span>
                  <span class="text-xs text-txtsecondary ml-1">({formatLatency(node.port22LatencyMs, node.isLocal)})</span>
                </td>
                <td class="p-2 border-b border-card-border">
                  <span class={node.sshOk ? "text-green-300" : "text-error"}>
                    {node.sshOk ? "OK" : "FAIL"}
                  </span>
                  <span class="text-xs text-txtsecondary ml-1">({formatLatency(node.sshLatencyMs, node.isLocal)})</span>
                </td>
                <td class="p-2 border-b border-card-border">
                  {#if node.dgx}
                    {#if node.dgx.supported}
                      <span class={node.dgx.updateAvailable ? "text-amber-300" : "text-green-300"}>
                        {node.dgx.updateAvailable ? "AVAILABLE" : "none"}
                      </span>
                      {#if node.dgx.rebootRunning}
                        <div class="text-xs text-amber-200">reboot in progress</div>
                      {/if}
                    {:else}
                      <span class="text-txtsecondary">n/a</span>
                    {/if}
                  {:else if dgxLoading}
                    <span class="text-txtsecondary">loading...</span>
                  {:else}
                    <span class="text-txtsecondary">-</span>
                  {/if}
                </td>
                <td class="p-2 border-b border-card-border">
                  {#if isNodeDGXUpdatable(node)}
                    <button
                      class="btn btn--sm"
                      onclick={() => runDgxUpdateNode(node.ip)}
                      disabled={dgxUpdating}
                      title={`UpdateAndReboot en ${node.ip}`}
                    >
                      {isNodeUpdating(node.ip) ? "Updating..." : "Update"}
                    </button>
                  {:else}
                    <span class="text-txtsecondary">-</span>
                  {/if}
                </td>
                <td class="p-2 border-b border-card-border text-xs">
                  {#if node.dgx?.supported}
                    <div>upgrade: {formatProgress(node.dgx.upgradeProgress, node.dgx.upgradeStatus)}</div>
                    <div>cache: {formatProgress(node.dgx.cacheProgress, node.dgx.cacheStatus)}</div>
                    <div class="text-txtsecondary">{formatTime(node.dgx.checkedAt)}</div>
                  {:else if node.dgx?.error}
                    <span class="text-error">{node.dgx.error}</span>
                  {:else if dgxLoading}
                    <span class="text-txtsecondary">loading...</span>
                  {:else}
                    <span class="text-txtsecondary">-</span>
                  {/if}
                </td>
                <td class="p-2 border-b border-card-border break-words">
                  {#if node.error || node.port22Error || node.sshError}
                    <div>{node.error || "-"}</div>
                    {#if node.port22Error}
                      <div class="text-xs text-txtsecondary">port22: {node.port22Error}</div>
                    {/if}
                    {#if node.sshError}
                      <div class="text-xs text-txtsecondary">ssh: {node.sshError}</div>
                    {/if}
                  {:else}
                    -
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else}
      <div class="text-sm text-txtsecondary">No hay datos de cluster.</div>
    {/if}
  </div>
</div>
