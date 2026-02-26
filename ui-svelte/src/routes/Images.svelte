<script lang="ts">
  import { onMount } from "svelte";
  import {
    deleteDockerImage,
    getDockerImages,
    getRecipeBackendActionStatus,
    getRecipeBackendState,
    runRecipeBackendAction,
    updateDockerImage,
  } from "../stores/api";
  import type {
    DockerImageInfo,
    DockerNodeImagesState,
    RecipeBackendActionStatus,
    RecipeBackendState,
  } from "../lib/types";

  let loading = true;
  let refreshing = false;
  let error: string | null = null;
  let notice: string | null = null;
  let discoveryError: string | null = null;

  let state: RecipeBackendState | null = null;
  let backendActionStatus: RecipeBackendActionStatus | null = null;
  let actionStatusTimer: ReturnType<typeof setInterval> | null = null;
  let dockerImages: DockerImageInfo[] = [];
  let nodeImages: DockerNodeImagesState[] = [];
  let actionRunning = "";
  let imageActionRunning = "";
  let actionCommand = "";
  let actionOutput = "";
  let selectedImage = "";

  function buildAction(next: RecipeBackendState | null): RecipeBackendState["actions"][number] | null {
    if (!next) return null;
    return next.actions.find((info) => info.action === "build_llamacpp") || null;
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

  function isBackendActionRunning(action?: string): boolean {
    if (!backendActionStatus?.running) return false;
    if (!action) return true;
    return backendActionStatus.action === action;
  }

  function runningLabel(action: string): string {
    if (action === "build_llamacpp") return "Building and copying image...";
    return "Running...";
  }

  function isImagesBackendAction(status: RecipeBackendActionStatus | null): boolean {
    const action = status?.action || "";
    return action === "build_llamacpp";
  }

  function actionStateLabel(state?: string): string {
    if (!state) return "idle";
    return state.toLowerCase();
  }

  function actionStateClass(status: RecipeBackendActionStatus): string {
    if (status.running) return "text-yellow-300";
    if ((status.state || "").toLowerCase() === "success") return "text-green-300";
    if ((status.state || "").toLowerCase() === "failed") return "text-error";
    return "text-txtmain";
  }

  function formatDuration(durationMs?: number): string {
    if (!durationMs || durationMs <= 0) return "0s";
    return `${Math.round(durationMs / 1000)}s`;
  }

  function imageActionKey(action: "update" | "delete", nodeIp: string, image: DockerImageInfo): string {
    return `${action}:${nodeIp}:${image.id || image.reference}`;
  }

  function isImageActionBusy(action: "update" | "delete", nodeIp: string, image: DockerImageInfo): boolean {
    return imageActionRunning === imageActionKey(action, nodeIp, image);
  }

  function canUpdateImage(image: DockerImageInfo): boolean {
    const reference = (image.reference || "").trim();
    if (!reference || reference.includes("<none>")) return false;
    if ((image.repository || "").trim() === "<none>") return false;
    if ((image.tag || "").trim() === "<none>") return false;
    return true;
  }

  async function refreshBackendActionStatus(signal?: AbortSignal): Promise<void> {
    try {
      backendActionStatus = await getRecipeBackendActionStatus(signal);
    } catch {
      // keep UI usable when endpoint is unavailable
    }
  }

  async function refresh(forceRefresh = false): Promise<void> {
    refreshing = true;
    error = null;
    notice = null;
    discoveryError = null;
    if (!state) loading = true;
    try {
      const [backendState, imagesState] = await Promise.all([
        getRecipeBackendState(),
        getDockerImages(forceRefresh),
      ]);
      state = backendState;
      dockerImages = imagesState.images || [];
      nodeImages = imagesState.nodes || [];
      discoveryError = imagesState.discoveryError || null;
      if (!selectedImage.trim()) {
        selectedImage = backendState.llamacppImage?.selected || "llama-cpp-spark:last";
      }
      await refreshBackendActionStatus();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      refreshing = false;
      loading = false;
    }
  }

  async function buildLlamaCpp(): Promise<void> {
    if (actionRunning || imageActionRunning || isBackendActionRunning("build_llamacpp")) return;

    const sourceImage = selectedImage.trim();
    if (!sourceImage) {
      error = "Selecciona o escribe una imagen Docker.";
      return;
    }

    actionRunning = "build_llamacpp";
    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";
    const startedAt = new Date().toISOString();
    backendActionStatus = {
      running: true,
      action: "build_llamacpp",
      state: "running",
      startedAt,
      updatedAt: startedAt,
      command: buildAction(state)?.commandHint || "build_llamacpp",
      output: "",
      error: "",
    };
    try {
      const result = await runRecipeBackendAction("build_llamacpp", { sourceImage });
      const successMessage = result.message || "Build y sincronización completados.";
      backendActionStatus = {
        running: false,
        action: "build_llamacpp",
        backendDir: result.backendDir,
        command: result.command || backendActionStatus?.command,
        state: "success",
        startedAt: backendActionStatus?.startedAt || startedAt,
        updatedAt: new Date().toISOString(),
        durationMs: result.durationMs,
        output: result.output || "",
        error: "",
      };
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      await refresh(true);
      notice = successMessage;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
      await refreshBackendActionStatus();
    } finally {
      actionRunning = "";
    }
  }

  async function updateNodeImage(node: DockerNodeImagesState, image: DockerImageInfo): Promise<void> {
    if (!canUpdateImage(image)) {
      error = `La imagen ${image.reference} no es actualizable (tag/repository inválido).`;
      return;
    }
    if (actionRunning || imageActionRunning || isBackendActionRunning()) return;

    imageActionRunning = imageActionKey("update", node.nodeIp, image);
    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";
    try {
      const result = await updateDockerImage(node.nodeIp, image.reference);
      const successMessage = result.message || `Imagen actualizada en ${result.nodeIp}`;
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      await refresh(true);
      notice = successMessage;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      imageActionRunning = "";
    }
  }

  async function deleteNodeImage(node: DockerNodeImagesState, image: DockerImageInfo): Promise<void> {
    if (actionRunning || imageActionRunning || isBackendActionRunning()) return;

    const target = (image.id || image.reference || "").trim();
    if (!target) {
      error = "No se encontró ID o referencia para borrar la imagen.";
      return;
    }

    imageActionRunning = imageActionKey("delete", node.nodeIp, image);
    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";
    try {
      const result = await deleteDockerImage(node.nodeIp, { id: image.id, reference: image.reference });
      const successMessage = result.message || `Imagen eliminada en ${result.nodeIp}`;
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      await refresh(true);
      notice = successMessage;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      imageActionRunning = "";
    }
  }

  onMount(() => {
    void refresh();
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

<div class="h-full flex flex-col gap-2">
  <div class="card shrink-0">
    <div class="flex items-center justify-between gap-2">
      <h2 class="pb-0">Images</h2>
      <button class="btn btn--sm" onclick={() => refresh(true)} disabled={refreshing || !!actionRunning || !!imageActionRunning}>
        {refreshing ? "Refreshing..." : "Refresh"}
      </button>
    </div>

    {#if error}
      <div class="mt-2 p-2 border border-error/40 bg-error/10 rounded text-sm text-error break-words">{error}</div>
    {/if}
    {#if notice}
      <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words">{notice}</div>
    {/if}
    {#if discoveryError}
      <div class="mt-2 p-2 border border-yellow-500/40 bg-yellow-600/10 rounded text-sm text-yellow-200 break-words">
        Cluster autodiscovery warning: {discoveryError}
      </div>
    {/if}
  </div>

  <div class="card shrink-0">
    <div class="text-sm text-txtsecondary mb-2">Construir imagen llama.cpp y copiar en nodos</div>
      <div class="space-y-2">
        {#if llamacppImageOptions(state).length > 0}
        <select class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={selectedImage}>
          {#each llamacppImageOptions(state) as image}
            <option value={image}>{image}</option>
          {/each}
        </select>
        {/if}
        <input class="input w-full font-mono text-sm" bind:value={selectedImage} placeholder="llama-cpp-spark:last" />

        <button class="btn btn--sm" onclick={buildLlamaCpp} disabled={!!actionRunning || !!imageActionRunning || isBackendActionRunning()}>
          {(actionRunning === "build_llamacpp" || isBackendActionRunning("build_llamacpp"))
            ? runningLabel("build_llamacpp")
            : (buildAction(state)?.label || "Build llama.cpp")}
        </button>
        <div class="text-xs text-txtsecondary break-all">
          Build:
          <span class="font-mono">{buildAction(state)?.commandHint || "POST /api/recipes/backend/action { action: build_llamacpp, sourceImage: <selected> }"}</span>
        </div>

      </div>

    {#if backendActionStatus && isImagesBackendAction(backendActionStatus)}
      <div class="mt-2 p-2 border border-card-border rounded bg-background/60 space-y-1">
        <div class="text-xs text-txtsecondary">
          Estado:
          <span class={`font-mono ${actionStateClass(backendActionStatus)}`}>{actionStateLabel(backendActionStatus.state)}</span>
          {#if backendActionStatus.durationMs}
            | duración: <span class="font-mono">{formatDuration(backendActionStatus.durationMs)}</span>
          {/if}
        </div>
        <div class="text-xs text-txtsecondary">
          Acción: <span class="font-mono">{backendActionStatus.action}</span>
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

    {#if actionCommand}
      <div class="mt-2 text-xs text-txtsecondary break-all">
        Último comando: <span class="font-mono">{actionCommand}</span>
      </div>
    {/if}
    {#if actionOutput}
      <pre class="mt-2 p-2 border border-card-border rounded bg-background/60 text-xs font-mono whitespace-pre-wrap break-all max-h-72 overflow-auto">{actionOutput}</pre>
    {/if}
  </div>

  <div class="card flex-1 min-h-0 flex flex-col">
    <div class="text-sm text-txtsecondary mb-2">Imágenes Docker por nodo</div>
    <div class="flex-1 min-h-0 overflow-auto">
    {#if loading}
      <div class="text-xs text-txtsecondary">Leyendo imágenes Docker...</div>
    {:else if nodeImages.length === 0}
      <div class="text-xs text-txtsecondary mb-2">No hay detalle por nodo, mostrando local.</div>
      {#if dockerImages.length === 0}
        <div class="text-xs text-txtsecondary">No hay imágenes Docker locales.</div>
      {:else}
        <div class="space-y-2">
          {#each dockerImages as image (image.id + image.reference)}
            <div class="p-2 border border-card-border rounded bg-background/40">
              <div class="text-sm font-mono text-txtmain break-all">{image.reference}</div>
              <div class="text-xs text-txtsecondary break-all">ID: {image.id}</div>
              <div class="text-xs text-txtsecondary">Size: {image.size} | Created: {image.createdSince}</div>
            </div>
          {/each}
        </div>
      {/if}
    {:else}
      <div class="grid gap-3 xl:grid-cols-2">
        {#each nodeImages as node (node.nodeIp)}
          <div class="p-2 border border-card-border rounded bg-background/30 flex flex-col min-h-0">
            <div class="flex flex-wrap items-center justify-between gap-2 mb-2">
              <div class="text-sm font-mono text-txtmain break-all">
                {node.nodeIp}{node.isLocal ? " (local)" : ""}
              </div>
              <div class="text-xs text-txtsecondary">{node.images.length} image(s)</div>
            </div>

            {#if node.error}
              <div class="text-xs text-error break-all">{node.error}</div>
            {:else if node.images.length === 0}
              <div class="text-xs text-txtsecondary">No hay imágenes en este nodo.</div>
            {:else}
              <div class="space-y-2 pr-1 max-h-[24rem] overflow-auto">
                {#each node.images as image (node.nodeIp + image.id + image.reference)}
                  <div class="p-2 border border-card-border rounded bg-background/40">
                    <div class="text-sm font-mono text-txtmain break-all">{image.reference}</div>
                    <div class="text-xs text-txtsecondary break-all">ID: {image.id}</div>
                    <div class="text-xs text-txtsecondary">Size: {image.size} | Created: {image.createdSince}</div>
                    <div class="mt-2 flex flex-wrap gap-2">
                      <button
                        class="btn btn--sm"
                        onclick={() => updateNodeImage(node, image)}
                        disabled={!!actionRunning || !!imageActionRunning || isBackendActionRunning() || !canUpdateImage(image)}
                        title={canUpdateImage(image) ? "Descargar/actualizar esta imagen en el nodo" : "Esta imagen no tiene referencia pullable"}
                      >
                        {isImageActionBusy("update", node.nodeIp, image) ? "Updating..." : "Update"}
                      </button>
                      <button
                        class="btn btn--sm"
                        onclick={() => deleteNodeImage(node, image)}
                        disabled={!!actionRunning || !!imageActionRunning || isBackendActionRunning()}
                        title="Eliminar esta imagen del nodo"
                      >
                        {isImageActionBusy("delete", node.nodeIp, image) ? "Deleting..." : "Delete"}
                      </button>
                    </div>
                  </div>
                {/each}
              </div>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
    </div>
  </div>
</div>
