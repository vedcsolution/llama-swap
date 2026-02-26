<script lang="ts">
  import { onMount } from "svelte";
  import {
    deleteRecipeBackendHFModel,
    generateRecipeBackendHFModel,
    getRecipeBackendActionStatus,
    getRecipeBackendHFModels,
    getRecipeBackendState,
    setRecipeBackendHFHubPath,
    runRecipeBackendAction,
  } from "../stores/api";
  import type {
    RecipeBackendActionStatus,
    RecipeBackendHFModel,
    RecipeBackendState,
  } from "../lib/types";
  import { collapseHomePath } from "../lib/pathDisplay";

  let loading = true;
  let error: string | null = null;
  let notice: string | null = null;

  let state: RecipeBackendState | null = null;
  let backendActionStatus: RecipeBackendActionStatus | null = null;
  let actionStatusTimer: ReturnType<typeof setInterval> | null = null;
  let downloadSubmitting = false;

  let hfModelName = "unsloth/Qwen3-Coder-Next-GGUF";
  let hfFormat: "gguf" | "safetensors" = "gguf";
  let hfQuantization = "Q8_0";

  let hfModelsLoading = false;
  let deletingHFModel = "";
  let generatingHFModel = "";
  let hfHubPath = "";
  let hfHubPathInput = "";
  let savingHFHubPath = false;
  let hfModels: RecipeBackendHFModel[] = [];

  function hfDownloadAction(next: RecipeBackendState | null): RecipeBackendState["actions"][number] | null {
    if (!next) return null;
    return next.actions.find((info) => info.action === "download_hf_model") || null;
  }

  function isBackendActionRunning(action?: string): boolean {
    if (!backendActionStatus?.running) return false;
    if (!action) return true;
    return backendActionStatus.action === action;
  }

  function isDownloadBusy(): boolean {
    return downloadSubmitting || isBackendActionRunning("download_hf_model");
  }

  function isHFModelActionDisabled(): boolean {
    return !!deletingHFModel || !!generatingHFModel || savingHFHubPath || isBackendActionRunning("download_hf_model");
  }

  function hasExistingHFRecipe(model: RecipeBackendHFModel): boolean {
    return !!model.hasRecipe;
  }

  function runningLabel(action: string): string {
    if (action === "download_hf_model") return "Downloading HF model...";
    return "Running...";
  }

  function downloadStateLabel(status: RecipeBackendActionStatus): string {
    if (status.running) return "running";
    return (status.state || "idle").toLowerCase();
  }

  function downloadStateClass(status: RecipeBackendActionStatus): string {
    if (status.running) return "text-yellow-300";
    if ((status.state || "").toLowerCase() === "success") return "text-green-300";
    if ((status.state || "").toLowerCase() === "failed") return "text-error";
    return "text-txtmain";
  }

  function previewDownloadCommand(hfModel: string): string {
    const base = hfDownloadAction(state)?.commandHint || "~/swap-laboratories/hf-download.sh <model>";
    return base.replace("<model>", hfModel);
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

  async function refreshBackendActionStatus(signal?: AbortSignal): Promise<void> {
    try {
      backendActionStatus = await getRecipeBackendActionStatus(signal);
    } catch {
      // keep UI usable when endpoint is unavailable
    }
  }

  async function refreshHFModels(signal?: AbortSignal): Promise<void> {
    hfModelsLoading = true;
    try {
      const next = await getRecipeBackendHFModels(signal);
      hfHubPath = next.hubPath || "";
      hfHubPathInput = hfHubPath;
      hfModels = next.models || [];
    } catch {
      hfHubPath = "";
      hfHubPathInput = "";
      hfModels = [];
    } finally {
      hfModelsLoading = false;
    }
  }

  async function refreshState(signal?: AbortSignal): Promise<void> {
    loading = true;
    error = null;
    try {
      state = await getRecipeBackendState(signal);
      await refreshHFModels(signal);
      await refreshBackendActionStatus(signal);
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  async function runDownload(): Promise<void> {
    if (isDownloadBusy()) {
      error = "Ya hay una descarga en progreso. Espera a que termine.";
      return;
    }

    const hfModel = hfModelName.trim();
    if (!hfModel) {
      error = "Introduce el nombre del modelo de Hugging Face (org/model).";
      return;
    }

    error = null;
    notice = null;
    downloadSubmitting = true;
    const startedAt = new Date().toISOString();
    backendActionStatus = {
      running: true,
      action: "download_hf_model",
      state: "running",
      startedAt,
      updatedAt: startedAt,
      command: previewDownloadCommand(hfModel),
      output: "",
      error: "",
    };
    try {
      const result = await runRecipeBackendAction("download_hf_model", {
        hfModel,
        hfFormat,
        hfQuantization: hfQuantization.trim(),
      });
      notice = result.message || "Descarga completada.";
      backendActionStatus = {
        running: false,
        action: "download_hf_model",
        backendDir: result.backendDir,
        command: result.command || backendActionStatus?.command,
        state: "success",
        startedAt: backendActionStatus?.startedAt || startedAt,
        updatedAt: new Date().toISOString(),
        durationMs: result.durationMs,
        output: result.output || "",
        error: "",
      };
      await refreshHFModels();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      downloadSubmitting = false;
      await refreshBackendActionStatus();
    }
  }

  async function deleteHFModel(model: RecipeBackendHFModel): Promise<void> {
    if (deletingHFModel) return;
    const target = model.modelId || model.cacheDir;

    deletingHFModel = model.cacheDir;
    error = null;
    notice = null;
    try {
      const next = await deleteRecipeBackendHFModel(model.cacheDir);
      hfHubPath = next.hubPath || "";
      hfHubPathInput = hfHubPath;
      hfModels = next.models || [];
      notice = `Modelo eliminado: ${target}`;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      await refreshHFModels();
      const stillExists = hfModels.some((item) => item.cacheDir === model.cacheDir);
      if (stillExists) {
        error = msg;
      } else {
        notice = `Modelo eliminado localmente: ${target}. Verifica nodos remotos si aplica.`;
        error = msg;
      }
    } finally {
      deletingHFModel = "";
    }
  }

  async function generateHFRecipe(model: RecipeBackendHFModel): Promise<void> {
    if (hasExistingHFRecipe(model)) {
      notice = `El modelo ${model.modelId || model.cacheDir} ya tiene receta (${model.existingRecipeRef || "asociada"}).`;
      error = null;
      return;
    }
    if (isHFModelActionDisabled()) return;

    generatingHFModel = model.cacheDir;
    error = null;
    notice = null;
    try {
      const result = await generateRecipeBackendHFModel(model.cacheDir);
      const action = result.createdRecipe ? "creada" : "reutilizada";
      notice = `Receta ${action}: ${result.recipeRef} (${result.format}) y modelo añadido en config.yaml como ${result.modelEntryId}.`;
      await refreshHFModels();
      await refreshBackendActionStatus();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      generatingHFModel = "";
    }
  }

  async function saveHFHubPath(): Promise<void> {
    if (savingHFHubPath) return;
    savingHFHubPath = true;
    error = null;
    notice = null;
    try {
      const next = await setRecipeBackendHFHubPath(hfHubPathInput.trim());
      hfHubPath = next.hubPath || "";
      hfHubPathInput = hfHubPath;
      hfModels = next.models || [];
      notice = `Ruta HF guardada: ${hfHubPath || "ruta por defecto"}`;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      savingHFHubPath = false;
    }
  }

  async function resetHFHubPath(): Promise<void> {
    if (savingHFHubPath) return;
    savingHFHubPath = true;
    error = null;
    notice = null;
    try {
      const next = await setRecipeBackendHFHubPath("");
      hfHubPath = next.hubPath || "";
      hfHubPathInput = hfHubPath;
      hfModels = next.models || [];
      notice = `Ruta HF restablecida: ${hfHubPath || "ruta por defecto"}`;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      savingHFHubPath = false;
    }
  }

  onMount(() => {
    void refreshState();
    actionStatusTimer = setInterval(() => {
      void refreshBackendActionStatus();
    }, 1500);
    return () => {
      if (actionStatusTimer) {
        clearInterval(actionStatusTimer);
        actionStatusTimer = null;
      }
    };
  });
</script>

<div class="card h-full flex flex-col min-h-0">
  <div class="flex items-center justify-between gap-2 mb-2">
    <h3>HF Models</h3>
    <button class="btn btn--sm" onclick={() => refreshState()} disabled={loading}>
      {loading ? "Refreshing..." : "Refresh"}
    </button>
  </div>

  {#if error}
    <div class="mb-2 p-2 border border-error/40 bg-error/10 rounded text-sm text-error break-words">{error}</div>
  {/if}
  {#if notice}
    <div class="mb-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words">{notice}</div>
  {/if}

  {#if hfDownloadAction(state)}
    <div class="mb-3 p-3 border border-card-border rounded bg-background/40 space-y-2">
      <div class="text-sm text-txtsecondary">Descargar modelo desde Hugging Face</div>
      <input class="input w-full font-mono text-sm" bind:value={hfModelName} placeholder="unsloth/Qwen3-Coder-Next-GGUF" />

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

      <button class="btn btn--sm" onclick={runDownload} disabled={isBackendActionRunning("download_hf_model") || !!deletingHFModel || !!generatingHFModel || savingHFHubPath}>
        {isDownloadBusy() ? runningLabel("download_hf_model") : (hfDownloadAction(state)?.label || "Download HF Model")}
      </button>
      <div class="text-xs text-txtsecondary break-all">
        Command:
        <span class="font-mono">{hfDownloadAction(state)?.commandHint || "~/swap-laboratories/hf-download.sh <model> --format <gguf|safetensors> [--quantization Q8_0|4bit] -c --copy-parallel"}</span>
      </div>
      {#if backendActionStatus && backendActionStatus.action === "download_hf_model"}
        <div class="p-2 border border-card-border rounded bg-background/60 space-y-1">
          <div class="text-xs text-txtsecondary">
            Estado:
            <span class={`font-mono ${downloadStateClass(backendActionStatus)}`}>{downloadStateLabel(backendActionStatus)}</span>
            {#if backendActionStatus.durationMs}
              | duración: <span class="font-mono">{Math.round(backendActionStatus.durationMs / 1000)}s</span>
            {/if}
          </div>
          {#if backendActionStatus.startedAt}
            <div class="text-xs text-txtsecondary">
              Inicio: <span class="font-mono">{backendActionStatus.startedAt}</span>
            </div>
          {/if}
          {#if backendActionStatus.updatedAt}
            <div class="text-xs text-txtsecondary">
              Actualizado: <span class="font-mono">{backendActionStatus.updatedAt}</span>
            </div>
          {/if}
          {#if backendActionStatus.command}
            <div class="text-xs text-txtsecondary break-all">
              Ejecución: <span class="font-mono">{backendActionStatus.command}</span>
            </div>
          {/if}
          {#if backendActionStatus.error}
            <div class="text-xs text-error break-all">Error: {backendActionStatus.error}</div>
          {/if}
          {#if backendActionStatus.output}
            <pre class="text-xs font-mono whitespace-pre-wrap break-all p-2 border border-card-border rounded bg-background/70 max-h-40 overflow-auto">{backendActionStatus.output}</pre>
          {/if}
        </div>
      {/if}
    </div>
  {:else}
    <div class="mb-3 text-xs text-txtsecondary">
      La descarga HF no está disponible para el backend actual.
    </div>
  {/if}

  <div class="p-3 border border-card-border rounded bg-background/40 flex-1 min-h-0 flex flex-col gap-2">
    <div class="text-sm text-txtsecondary">Modelos descargados (Hugging Face)</div>
    <div class="text-xs text-txtsecondary">Ruta del hub</div>
    <div class="flex flex-col md:flex-row gap-2">
      <input class="input w-full font-mono text-sm" bind:value={hfHubPathInput} placeholder="~/.cache/huggingface/hub" />
      <button class="btn btn--sm" onclick={saveHFHubPath} disabled={savingHFHubPath || hfModelsLoading || !!deletingHFModel || !!generatingHFModel || isBackendActionRunning("download_hf_model")}>
        {savingHFHubPath ? "Guardando..." : "Guardar ruta"}
      </button>
      <button class="btn btn--sm" onclick={resetHFHubPath} disabled={savingHFHubPath || hfModelsLoading || !!deletingHFModel || !!generatingHFModel || isBackendActionRunning("download_hf_model")}>
        Restablecer
      </button>
    </div>
    <div class="text-xs text-txtsecondary break-all">
      Cache:
      <span class="font-mono">{hfHubPath || "~/.cache/huggingface/hub"}</span>
    </div>
    {#if hfModelsLoading}
      <div class="text-xs text-txtsecondary">Leyendo modelos descargados...</div>
    {:else if hfModels.length === 0}
      <div class="text-xs text-txtsecondary">No hay modelos descargados.</div>
    {:else}
      <div class="space-y-2 overflow-auto pr-1 flex-1 min-h-0">
        {#each hfModels as model (model.cacheDir)}
          <div class="p-2 border border-card-border rounded bg-background/60">
            <div class="text-sm font-mono text-txtmain break-all">{model.modelId || model.cacheDir}</div>
            <div class="text-xs text-txtsecondary break-all">{collapseHomePath(model.path)}</div>
            <div class="text-xs text-txtsecondary">Tamaño: {formatBytes(model.sizeBytes)} | Actualizado: {model.modifiedAt}</div>
            <div class="mt-2 flex flex-wrap gap-2">
              <button
                class="btn btn--sm"
                onclick={() => generateHFRecipe(model)}
                disabled={isHFModelActionDisabled() || hasExistingHFRecipe(model)}
                title={hasExistingHFRecipe(model)
                  ? `Receta existente: ${model.existingRecipeRef || "asociada"}`
                  : `Generar receta para ${model.modelId || model.cacheDir}`}
              >
                {generatingHFModel === model.cacheDir
                  ? "Generando..."
                  : hasExistingHFRecipe(model)
                    ? "Receta creada"
                    : "Generar receta"}
              </button>
              <button
                class="btn btn--sm"
                onclick={() => deleteHFModel(model)}
                disabled={isHFModelActionDisabled()}
                title={`Eliminar ${model.modelId || model.cacheDir}`}
              >
                {deletingHFModel === model.cacheDir ? "Eliminando..." : "Eliminar"}
              </button>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </div>
</div>
