<script lang="ts">
  import { onMount } from "svelte";
  import {
    deleteRecipeBackendHFModel,
    getRecipeBackendActionStatus,
    getRecipeBackendHFModels,
    getRecipeBackendState,
    runRecipeBackendAction,
  } from "../stores/api";
  import type {
    RecipeBackendAction,
    RecipeBackendActionStatus,
    RecipeBackendHFModel,
    RecipeBackendState,
  } from "../lib/types";
  import { collapseHomePath } from "../lib/pathDisplay";

  let loading = true;
  let refreshing = false;
  let error: string | null = null;
  let notice: string | null = null;
  let state: RecipeBackendState | null = null;
  let actionRunning = "";
  let actionCommand = "";
  let actionOutput = "";

  let selectedTrtllmImage = "";
  let selectedNvidiaImage = "";
  let selectedLlamacppImage = "";

  let hfModelName = "QuantTrio/MiniMax-M2-AWQ";
  let hfFormat: "gguf" | "safetensors" = "safetensors";
  let hfQuantization = "";

  let hfModelsLoading = false;
  let deletingHFModel = "";
  let hfHubPath = "";
  let hfModels: RecipeBackendHFModel[] = [];

  let backendActionStatus: RecipeBackendActionStatus | null = null;
  let actionStatusTimer: ReturnType<typeof setInterval> | null = null;
  let refreshController: AbortController | null = null;

  function sourceLabel(source: RecipeBackendState["backendSource"]): string {
    if (source === "override") return "override (UI)";
    if (source === "env") return "env";
    return "default";
  }

  function backendKindLabel(kind: RecipeBackendState["backendKind"], vendor?: string): string {
    const v = (vendor || "").trim();
    if (!v) return kind;
    return `${kind} (${v})`;
  }

  function syncSelectionFromState(next: RecipeBackendState): void {
    selectedTrtllmImage = next.trtllmImage?.selected || "";
    selectedNvidiaImage = next.nvidiaImage?.selected || "";
    selectedLlamacppImage = next.llamacppImage?.selected || "";
  }

  function trtllmImageOptions(next: RecipeBackendState | null): string[] {
    if (!next?.trtllmImage) return [];
    const out: string[] = [];
    const push = (v?: string) => {
      const value = (v || "").trim();
      if (!value || out.includes(value)) return;
      out.push(value);
    };

    push(next.trtllmImage.selected);
    push(next.trtllmImage.default);
    push(next.trtllmImage.latest);
    for (const img of next.trtllmImage.available || []) push(img);
    return out;
  }

  function nvidiaImageOptions(next: RecipeBackendState | null): string[] {
    if (!next?.nvidiaImage) return [];
    const out: string[] = [];
    const push = (v?: string) => {
      const value = (v || "").trim();
      if (!value || out.includes(value)) return;
      out.push(value);
    };

    push(next.nvidiaImage.selected);
    push(next.nvidiaImage.default);
    push(next.nvidiaImage.latest);
    for (const img of next.nvidiaImage.available || []) push(img);
    return out;
  }

  function llamacppImageOptions(next: RecipeBackendState | null): string[] {
    if (!next?.llamacppImage) return [];
    const out: string[] = [];
    const push = (v?: string) => {
      const value = (v || "").trim();
      if (!value || out.includes(value)) return;
      out.push(value);
    };

    push(next.llamacppImage.selected);
    push(next.llamacppImage.default);
    for (const img of next.llamacppImage.available || []) push(img);
    return out;
  }

  function hfDownloadAction(next: RecipeBackendState | null): RecipeBackendState["actions"][number] | null {
    if (!next) return null;
    return next.actions.find((info) => info.action === "download_hf_model") || null;
  }

  function backendActionsWithoutSpecialDownloads(next: RecipeBackendState | null): RecipeBackendState["actions"] {
    if (!next) return [];
    return next.actions.filter((info) => info.action !== "download_hf_model");
  }

  function runningLabel(action: string): string {
    switch (action) {
      case "git_pull":
        return "Running git pull...";
      case "git_pull_rebase":
        return "Running rebase pull...";
      case "build_vllm":
        return "Building vLLM...";
      case "build_mxfp4":
        return "Building MXFP4...";
      case "build_vllm_12_0f":
        return "Building 12.0f...";
      case "pull_trtllm_image":
        return "Pulling TRT-LLM image...";
      case "update_trtllm_image":
        return "Updating TRT-LLM image...";
      case "pull_nvidia_image":
        return "Pulling NVIDIA image...";
      case "update_nvidia_image":
        return "Updating NVIDIA image...";
      case "sync_llamacpp_image":
        return "Updating llama.cpp image on cluster...";
      case "download_hf_model":
        return "Downloading HF model...";
      default:
        return "Running...";
    }
  }

  function actionStateLabel(value: string | undefined): string {
    if (value === "success") return "success";
    if (value === "failed") return "failed";
    if (value === "running") return "running";
    return "idle";
  }

  function isBackendActionRunning(action?: string): boolean {
    if (!backendActionStatus?.running) return false;
    if (!action) return true;
    return backendActionStatus.action === action;
  }

  async function refreshBackendActionStatus(signal?: AbortSignal): Promise<void> {
    try {
      const previous = backendActionStatus;
      const next = await getRecipeBackendActionStatus(signal);
      backendActionStatus = next;

      if (next.command) actionCommand = next.command;
      if (!next.running && next.output) actionOutput = next.output;

      if (previous?.running && !next.running && next.action === "download_hf_model") {
        await refreshHFModels(signal);
        if (next.state === "success") {
          notice = `Descarga completada en ${formatDuration(next.durationMs || 0)}.`;
        } else if (next.state === "failed" && next.error) {
          error = next.error;
        }
      }
    } catch {
      // keep page usable when endpoint is unavailable
    }
  }

  function formatBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let value = bytes;
    let unitIndex = 0;
    while (value >= 1024 && unitIndex < units.length - 1) {
      value /= 1024;
      unitIndex += 1;
    }
    const digits = unitIndex === 0 ? 0 : value >= 100 ? 0 : value >= 10 ? 1 : 2;
    return `${value.toFixed(digits)} ${units[unitIndex]}`;
  }

  async function refreshHFModels(signal?: AbortSignal): Promise<void> {
    hfModelsLoading = true;
    try {
      const next = await getRecipeBackendHFModels(signal);
      hfHubPath = next.hubPath || "";
      hfModels = next.models || [];
    } catch {
      hfHubPath = "";
      hfModels = [];
    } finally {
      hfModelsLoading = false;
    }
  }

  async function deleteHFModel(model: RecipeBackendHFModel): Promise<void> {
    if (deletingHFModel) return;
    const target = model.modelId || model.cacheDir;
    if (!confirm(`Eliminar modelo descargado ${target}?`)) return;

    deletingHFModel = model.cacheDir;
    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";
    try {
      const next = await deleteRecipeBackendHFModel(model.cacheDir);
      hfHubPath = next.hubPath || "";
      hfModels = next.models || [];
      notice = `Modelo eliminado: ${target}`;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      deletingHFModel = "";
    }
  }

  async function refresh(): Promise<void> {
    refreshController?.abort();
    const controller = new AbortController();
    refreshController = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);

    refreshing = true;
    error = null;
    notice = null;
    if (!state) loading = true;
    try {
      const next = await getRecipeBackendState(controller.signal);
      state = next;
      syncSelectionFromState(next);
      await refreshHFModels(controller.signal);
      await refreshBackendActionStatus(controller.signal);
    } catch (e) {
      if (controller.signal.aborted) {
        error = "Timeout consultando backend. Pulsa Refresh para reintentar.";
      } else {
        error = e instanceof Error ? e.message : String(e);
      }
    } finally {
      clearTimeout(timeout);
      if (refreshController === controller) refreshController = null;
      refreshing = false;
      loading = false;
    }
  }

  function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms} ms`;
    return `${(ms / 1000).toFixed(1)} s`;
  }

  async function runAction(action: string, label: string): Promise<void> {
    const isDownload = action === "download_hf_model";
    if (!isDownload && actionRunning) return;
    if (isDownload && isBackendActionRunning("download_hf_model")) {
      error = "Ya hay una descarga en progreso. Espera a que termine.";
      return;
    }

    if (!isDownload) actionRunning = action;

    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";

    if (isDownload) {
      backendActionStatus = {
        running: true,
        action,
        state: "running",
        backendDir: state?.backendDir,
        startedAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      };
    }

    try {
      const sourceImage =
        action === "pull_trtllm_image" || action === "update_trtllm_image"
          ? selectedTrtllmImage.trim()
          : action === "pull_nvidia_image" || action === "update_nvidia_image"
            ? selectedNvidiaImage.trim()
            : action === "sync_llamacpp_image"
              ? selectedLlamacppImage.trim()
              : "";

      const hfModel = action === "download_hf_model" ? hfModelName.trim() : "";
      if (action === "download_hf_model" && !hfModel) {
        throw new Error("Introduce el nombre del modelo de Hugging Face (org/model).");
      }

      const opts =
        sourceImage || hfModel
          ? {
              sourceImage,
              hfModel,
              hfFormat,
              hfQuantization: hfQuantization.trim(),
            }
          : undefined;

      const result = await runRecipeBackendAction(action as RecipeBackendAction, opts);
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      notice = result.message || `${label} completado en ${formatDuration(result.durationMs || 0)}.`;

      if (isDownload) {
        await refreshBackendActionStatus();
      }

      if (
        action === "git_pull" ||
        action === "git_pull_rebase" ||
        action === "pull_trtllm_image" ||
        action === "update_trtllm_image" ||
        action === "pull_nvidia_image" ||
        action === "update_nvidia_image" ||
        action === "sync_llamacpp_image"
      ) {
        await refresh();
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
      await refreshBackendActionStatus();
    } finally {
      if (!isDownload) actionRunning = "";
    }
  }

  onMount(() => {
    void refresh();
    actionStatusTimer = setInterval(() => {
      void refreshBackendActionStatus();
    }, 3000);

    return () => {
      refreshController?.abort();
      if (actionStatusTimer) {
        clearInterval(actionStatusTimer);
        actionStatusTimer = null;
      }
    };
  });
</script>

<div class="h-full flex flex-col gap-2">
  <div class="card shrink-0">
    <div class="flex items-center justify-between gap-2">
      <h2 class="pb-0">Backend</h2>
      <button class="btn btn--sm" onclick={refresh} disabled={refreshing}>{refreshing ? "Refreshing..." : "Refresh"}</button>
    </div>

    {#if state}
      <div class="mt-2 text-sm text-txtsecondary break-all">
        Actual:
        <span class="font-mono text-txtmain" title={state.backendDir}>{collapseHomePath(state.backendDir)}</span>
      </div>
      <div class="text-xs text-txtsecondary">Fuente: {sourceLabel(state.backendSource)}</div>
      <div class="text-xs text-txtsecondary">
        Tipo backend:
        <span class="font-mono text-txtmain">{backendKindLabel(state.backendKind, state.backendVendor)}</span>
        {#if state.repoUrl}
          | repo: <span class="font-mono text-txtmain">{state.repoUrl}</span>
        {/if}
      </div>
    {/if}

    {#if error}
      <div class="mt-2 p-2 border border-error/40 bg-error/10 rounded text-sm text-error break-words">{error}</div>
    {/if}
    {#if notice}
      <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words">{notice}</div>
    {/if}
  </div>

  <div class="card flex-1 min-h-0 overflow-auto">
    {#if loading}
      <div class="text-sm text-txtsecondary">Cargando estado de backend...</div>
    {:else if state}
      {#if state.backendKind === "trtllm" && state.trtllmImage}
        <div class="mt-1 p-3 border border-card-border rounded bg-background/40 space-y-2">
          <div class="text-sm text-txtsecondary">TRT-LLM source image (NVIDIA)</div>
          {#if state.deploymentGuideUrl}
            <div class="text-xs text-txtsecondary break-all">
              Guía técnica:
              <a class="underline text-cyan-300" href={state.deploymentGuideUrl} target="_blank" rel="noreferrer">{state.deploymentGuideUrl}</a>
            </div>
          {/if}
          <select class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={selectedTrtllmImage}>
            {#each trtllmImageOptions(state) as image}
              <option value={image}>{image}</option>
            {/each}
          </select>
          <input
            class="input w-full font-mono text-sm"
            bind:value={selectedTrtllmImage}
            placeholder="nvcr.io/nvidia/tensorrt-llm/release:1.3.0rc3"
          />
          <div class="text-xs text-txtsecondary break-all">default: <span class="font-mono">{state.trtllmImage.default}</span></div>
          {#if state.trtllmImage.latest}
            <div class="text-xs text-txtsecondary break-all">
              latest: <span class="font-mono">{state.trtllmImage.latest}</span>
              {#if state.trtllmImage.updateAvailable}
                <span class="ml-2 text-amber-300 font-semibold">update available</span>
              {:else}
                <span class="ml-2 text-green-300">up-to-date</span>
              {/if}
            </div>
          {/if}
          {#if state.trtllmImage.warning}
            <div class="p-2 border border-amber-500/30 bg-amber-500/10 rounded text-xs text-amber-300 break-words">
              {state.trtllmImage.warning}
            </div>
          {/if}
          <div class="text-xs text-txtsecondary">Se guarda como preferencia de imagen para despliegues TRT-LLM.</div>
        </div>
      {/if}

      {#if state.backendKind === "nvidia" && state.nvidiaImage}
        <div class="mt-4 p-3 border border-card-border rounded bg-background/40 space-y-2">
          <div class="text-sm text-txtsecondary">NVIDIA vLLM image (vllm-openai)</div>
          {#if state.deploymentGuideUrl}
            <div class="text-xs text-txtsecondary break-all">
              Guía técnica:
              <a class="underline text-cyan-300" href={state.deploymentGuideUrl} target="_blank" rel="noreferrer">{state.deploymentGuideUrl}</a>
            </div>
          {/if}
          <select class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={selectedNvidiaImage}>
            {#each nvidiaImageOptions(state) as image}
              <option value={image}>{image}</option>
            {/each}
          </select>
          <input class="input w-full font-mono text-sm" bind:value={selectedNvidiaImage} placeholder="nvcr.io/nvidia/vllm:26.01-py3" />
          <div class="text-xs text-txtsecondary break-all">default: <span class="font-mono">{state.nvidiaImage.default}</span></div>
          {#if state.nvidiaImage.latest}
            <div class="text-xs text-txtsecondary break-all">
              latest: <span class="font-mono">{state.nvidiaImage.latest}</span>
              {#if state.nvidiaImage.updateAvailable}
                <span class="ml-2 text-amber-300 font-semibold">update available</span>
              {:else}
                <span class="ml-2 text-green-300">up-to-date</span>
              {/if}
            </div>
          {/if}
          {#if state.nvidiaImage.warning}
            <div class="p-2 border border-amber-500/30 bg-amber-500/10 rounded text-xs text-amber-300 break-words">
              {state.nvidiaImage.warning}
            </div>
          {/if}
          <div class="text-xs text-txtsecondary">Se guarda como preferencia de imagen para despliegues NVIDIA vLLM.</div>
        </div>
      {/if}

      {#if state.backendKind === "llamacpp" && state.llamacppImage}
        <div class="mt-4 p-3 border border-card-border rounded bg-background/40 space-y-2">
          <div class="text-sm text-txtsecondary">llama.cpp source image</div>
          {#if state.deploymentGuideUrl}
            <div class="text-xs text-txtsecondary break-all">
              Guía técnica:
              <a class="underline text-cyan-300" href={state.deploymentGuideUrl} target="_blank" rel="noreferrer">{state.deploymentGuideUrl}</a>
            </div>
          {/if}
          <select class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={selectedLlamacppImage}>
            {#each llamacppImageOptions(state) as image}
              <option value={image}>{image}</option>
            {/each}
          </select>
          <input class="input w-full font-mono text-sm" bind:value={selectedLlamacppImage} placeholder="llama-cpp-spark:last" />
          <div class="text-xs text-txtsecondary break-all">default: <span class="font-mono">{state.llamacppImage.default}</span></div>
          {#if state.llamacppImage.warning}
            <div class="p-2 border border-amber-500/30 bg-amber-500/10 rounded text-xs text-amber-300 break-words">
              {state.llamacppImage.warning}
            </div>
          {/if}
          <div class="text-xs text-txtsecondary">Se guarda como preferencia de imagen para despliegues llama.cpp.</div>
        </div>
      {/if}

      <div class="mt-4 pt-3 border-t border-card-border">
        <div class="text-sm text-txtsecondary mb-2">Backend actions</div>

        {#if isBackendActionRunning()}
          <div class="mb-3 p-2 border border-amber-500/30 bg-amber-500/10 rounded text-xs text-amber-300 break-words space-y-1">
            <div>
              Acción en progreso:
              <span class="font-mono">{runningLabel(backendActionStatus?.action || "")}</span>
            </div>
            <div>
              Inicio:
              <span class="font-mono">{backendActionStatus?.startedAt || "-"}</span>
            </div>
          </div>
        {:else if backendActionStatus?.action}
          <div class="mb-3 p-2 border border-card-border rounded bg-background/40 text-xs text-txtsecondary break-words space-y-1">
            <div>
              Última acción:
              <span class="font-mono">{backendActionStatus.action}</span>
            </div>
            <div>
              Estado:
              <span class="font-mono">{actionStateLabel(backendActionStatus.state)}</span>
              {#if backendActionStatus.durationMs}
                | duración: <span class="font-mono">{formatDuration(backendActionStatus.durationMs)}</span>
              {/if}
            </div>
            <div>
              Actualizado:
              <span class="font-mono">{backendActionStatus.updatedAt || "-"}</span>
            </div>
          </div>
        {/if}

        {#if hfDownloadAction(state)}
          <div class="mb-3 p-3 border border-card-border rounded bg-background/40 space-y-2">
            <div class="text-sm text-txtsecondary">Descargar modelo desde Hugging Face</div>
            <input class="input w-full font-mono text-sm" bind:value={hfModelName} placeholder="QuantTrio/MiniMax-M2-AWQ" />

            <div class="grid grid-cols-1 md:grid-cols-2 gap-2">
              <label class="text-xs text-txtsecondary">
                Formato
                <select class="mt-1 w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={hfFormat}>
                  <option value="safetensors">safetensors</option>
                  <option value="gguf">gguf</option>
                </select>
              </label>
              <label class="text-xs text-txtsecondary">
                Cuantización (opcional)
                <input class="mt-1 input w-full font-mono text-sm" bind:value={hfQuantization} placeholder={hfFormat === "gguf" ? "Q8_0 o Q4_K_M" : "4bit o awq"} />
              </label>
            </div>

            <button
              class="btn btn--sm"
              onclick={() => runAction("download_hf_model", hfDownloadAction(state)?.label || "Download HF Model")}
              disabled={isBackendActionRunning("download_hf_model") || !!deletingHFModel}
              title={hfDownloadAction(state)?.commandHint || "Download HF Model"}
            >
              {isBackendActionRunning("download_hf_model")
                ? runningLabel("download_hf_model")
                : (hfDownloadAction(state)?.label || "Download HF Model")}
            </button>
            <div class="text-xs text-txtsecondary break-all">
              Command:
              <span class="font-mono">{hfDownloadAction(state)?.commandHint || "~/swap-laboratories/hf-download.sh <model> --format <gguf|safetensors> [--quantization Q8_0|4bit] -c --copy-parallel"}</span>
            </div>
          </div>
        {/if}

        <div class="mb-3 p-3 border border-card-border rounded bg-background/40 space-y-2">
          <div class="text-sm text-txtsecondary">Modelos descargados (Hugging Face)</div>
          <div class="text-xs text-txtsecondary break-all">
            Cache:
            <span class="font-mono">{hfHubPath || "~/.cache/huggingface/hub"}</span>
          </div>
          {#if hfModelsLoading}
            <div class="text-xs text-txtsecondary">Leyendo modelos descargados...</div>
          {:else if hfModels.length === 0}
            <div class="text-xs text-txtsecondary">No hay modelos descargados.</div>
          {:else}
            <div class="space-y-2">
              {#each hfModels as model (model.cacheDir)}
                <div class="p-2 border border-card-border rounded bg-background/60">
                  <div class="text-sm font-mono text-txtmain break-all">{model.modelId || model.cacheDir}</div>
                  <div class="text-xs text-txtsecondary break-all">{collapseHomePath(model.path)}</div>
                  <div class="text-xs text-txtsecondary">Tamaño: {formatBytes(model.sizeBytes)} | Actualizado: {model.modifiedAt}</div>
                  <div class="mt-2">
                    <button class="btn btn--sm" onclick={() => deleteHFModel(model)} disabled={!!deletingHFModel} title={`Eliminar ${model.modelId || model.cacheDir}`}>
                      {deletingHFModel === model.cacheDir ? "Eliminando..." : "Eliminar"}
                    </button>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>

        {#if backendActionsWithoutSpecialDownloads(state).length === 0}
          {#if !hfDownloadAction(state)}
            <div class="text-xs text-txtsecondary">No hay acciones disponibles para este backend.</div>
          {/if}
        {:else}
          <div class="flex flex-wrap gap-2">
            {#each backendActionsWithoutSpecialDownloads(state) as info (info.action + info.label)}
              <button
                class="btn btn--sm"
                onclick={() => runAction(info.action, info.label)}
                disabled={!!actionRunning || refreshing || isBackendActionRunning()}
                title={info.commandHint || info.label}
              >
                {(actionRunning === info.action || isBackendActionRunning(info.action)) ? runningLabel(info.action) : info.label}
              </button>
            {/each}
          </div>
        {/if}

        {#if actionCommand}
          <div class="mt-2 text-xs text-txtsecondary break-all">
            Command: <span class="font-mono">{actionCommand}</span>
          </div>
        {/if}
        {#if actionOutput}
          <pre class="mt-2 p-2 border border-card-border rounded bg-background/60 text-xs font-mono whitespace-pre-wrap break-all max-h-72 overflow-auto">{actionOutput}</pre>
        {/if}
      </div>
    {:else}
      <div class="text-sm text-txtsecondary">No se pudo cargar el estado del backend.</div>
    {/if}
  </div>
</div>
