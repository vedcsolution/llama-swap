<script lang="ts">
  import { onMount } from "svelte";
  import { getClusterStatus, runClusterDGXUpdate } from "../stores/api";
  import type { ClusterStatusState } from "../lib/types";
  import { collapseHomePath } from "../lib/pathDisplay";

  let loading = true;
  let refreshing = false;
  let dgxUpdating = false;
  let dgxUpdatingTargets: Record<string, boolean> = {};
  let dgxUpdateConfirmKey = "";
  let error: string | null = null;
  let dgxActionError: string | null = null;
  let dgxActionResult: string | null = null;
  let state: ClusterStatusState | null = null;
  let refreshController: AbortController | null = null;

  function formatTime(value?: string): string {
    if (!value) return "unknown";
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return value;
    return parsed.toLocaleString();
  }

  function formatLatency(value?: number): string {
    if (value == null || value < 0) return "-";
    return `${value} ms`;
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

  type NodeMetricSummary = {
    percent: number | null;
    label: string;
    error?: string;
  };

  function clampPercent(value?: number | null): number {
    if (value == null || Number.isNaN(value)) return 0;
    if (value < 0) return 0;
    if (value > 100) return 100;
    return value;
  }

  function formatMiB(value?: number): string {
    if (value == null || value < 0) return "-";
    const gib = value / 1024;
    if (gib >= 100) return `${Math.round(gib)} GiB`;
    if (gib >= 10) return `${gib.toFixed(1)} GiB`;
    return `${gib.toFixed(2)} GiB`;
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

  function buildVramSummary(node: ClusterStatusState["nodes"][number]): NodeMetricSummary {
    if (node.gpu?.error) {
      return { percent: null, label: "-", error: node.gpu.error };
    }
    const devices = node.gpu?.devices || [];
    if (devices.length === 0) {
      return { percent: null, label: "sin GPU" };
    }
    const totalMiB = devices.reduce((sum, device) => sum + (device.totalMiB || 0), 0);
    const usedMiB = devices.reduce((sum, device) => sum + (device.usedMiB || 0), 0);
    if (totalMiB <= 0) {
      return { percent: null, label: `N/A (${devices.length} GPU)` };
    }
    const usage = Math.round((usedMiB / totalMiB) * 100);
    return { percent: usage, label: `${usage}% (${formatMiB(usedMiB)} / ${formatMiB(totalMiB)})` };
  }

  async function refresh(forceRefresh = false): Promise<void> {
    refreshController?.abort();
    const controller = new AbortController();
    refreshController = controller;
    const timeout = setTimeout(() => controller.abort(), 30000);

    refreshing = true;
    error = null;
    if (!state) loading = true;
    try {
      state = await getClusterStatus(controller.signal, forceRefresh);
    } catch (e) {
      if (controller.signal.aborted) {
        error = "Timeout al consultar el estado del cluster. Pulsa Refresh para reintentar.";
      } else {
        error = e instanceof Error ? e.message : String(e);
      }
    } finally {
      clearTimeout(timeout);
      if (refreshController === controller) {
        refreshController = null;
      }
      refreshing = false;
      loading = false;
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
    void refresh(false);
    return () => {
      refreshController?.abort();
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
          <div>SSH OK: {state.reachableBySsh}</div>
        </div>
      </div>

      <div class="mb-3">
        <div class="text-sm font-semibold text-txtmain">Recursos por nodo</div>
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
      {/if}

      <div class="overflow-auto border border-card-border rounded">
        <table class="w-full text-sm">
          <thead class="bg-surface">
            <tr>
              <th class="text-left p-2 border-b border-card-border">Nodo</th>
              <th class="text-left p-2 border-b border-card-border">Rol</th>
              <th class="text-left p-2 border-b border-card-border">Port 22</th>
              <th class="text-left p-2 border-b border-card-border">SSH BatchMode</th>
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
                  <span class="text-xs text-txtsecondary ml-1">({formatLatency(node.port22LatencyMs)})</span>
                </td>
                <td class="p-2 border-b border-card-border">
                  <span class={node.sshOk ? "text-green-300" : "text-error"}>
                    {node.sshOk ? "OK" : "FAIL"}
                  </span>
                  <span class="text-xs text-txtsecondary ml-1">({formatLatency(node.sshLatencyMs)})</span>
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
                  {:else}
                    <span class="text-txtsecondary">-</span>
                  {/if}
                </td>
                <td class="p-2 border-b border-card-border break-words">{node.error || "-"}</td>
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
