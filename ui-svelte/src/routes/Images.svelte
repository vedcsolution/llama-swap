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

  function syncAction(next: RecipeBackendState | null): RecipeBackendState["actions"][number] | null {
    if (!next) return null;
    return next.actions.find((info) => info.action === "sync_llamacpp_image") || null;
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
    if (action === "sync_llamacpp_image") return "Syncing image on nodes...";
    return "Running...";
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

  async function refresh(): Promise<void> {
    refreshing = true;
    error = null;
    notice = null;
    discoveryError = null;
    if (!state) loading = true;
    try {
      const [backendState, imagesState] = await Promise.all([
        getRecipeBackendState(),
        getDockerImages(),
      ]);
      state = backendState;
      dockerImages = imagesState.images || [];
      nodeImages = imagesState.nodes || [];
      discoveryError = imagesState.discoveryError || null;
      if (!selectedImage.trim()) {
        selectedImage = backendState.llamacppImage?.selected || "";
      }
      await refreshBackendActionStatus();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      refreshing = false;
      loading = false;
    }
  }

  async function syncImage(): Promise<void> {
    const action = syncAction(state);
    if (!action) {
      error = "La acción de sync no está disponible para el backend actual.";
      return;
    }
    if (actionRunning || imageActionRunning || isBackendActionRunning("sync_llamacpp_image")) return;

    const sourceImage = selectedImage.trim();
    if (!sourceImage) {
      error = "Selecciona o escribe una imagen Docker.";
      return;
    }

    actionRunning = "sync_llamacpp_image";
    error = null;
    notice = null;
    actionCommand = "";
    actionOutput = "";
    try {
      const result = await runRecipeBackendAction("sync_llamacpp_image", { sourceImage });
      const successMessage = result.message || "Imagen sincronizada.";
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      await refresh();
      notice = successMessage;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
      await refreshBackendActionStatus();
    } finally {
      actionRunning = "";
    }
  }

  async function buildLlamaCpp(): Promise<void> {
    const action = buildAction(state);
    if (!action) {
      error = "La acción de build no está disponible para el backend actual.";
      return;
    }
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
    try {
      const result = await runRecipeBackendAction("build_llamacpp", { sourceImage });
      const successMessage = result.message || "Build y sincronización completados.";
      actionCommand = result.command || "";
      actionOutput = result.output || "";
      await refresh();
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
      await refresh();
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
    const confirmed = confirm(`Eliminar imagen en ${node.nodeIp}: ${image.reference}?`);
    if (!confirmed) return;

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
      await refresh();
      notice = successMessage;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      imageActionRunning = "";
    }
  }

  onMount(() => {
    void refresh();
  });
</script>

<div class="h-full flex flex-col gap-2">
  <div class="card shrink-0">
    <div class="flex items-center justify-between gap-2">
      <h2 class="pb-0">Images</h2>
      <button class="btn btn--sm" onclick={refresh} disabled={refreshing || !!actionRunning || !!imageActionRunning}>
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
    <div class="text-sm text-txtsecondary mb-2">Descargar y sincronizar imagen en nodos</div>

    {#if syncAction(state)}
      <div class="space-y-2">
        <select class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono text-sm" bind:value={selectedImage}>
          {#each llamacppImageOptions(state) as image}
            <option value={image}>{image}</option>
          {/each}
        </select>
        <input class="input w-full font-mono text-sm" bind:value={selectedImage} placeholder="llama-cpp-spark:last" />

        {#if buildAction(state)}
          <button class="btn btn--sm" onclick={buildLlamaCpp} disabled={!!actionRunning || !!imageActionRunning || isBackendActionRunning()}>
            {(actionRunning === "build_llamacpp" || isBackendActionRunning("build_llamacpp"))
              ? runningLabel("build_llamacpp")
              : (buildAction(state)?.label || "Build llama.cpp")}
          </button>
          <div class="text-xs text-txtsecondary break-all">
            Build:
            <span class="font-mono">{buildAction(state)?.commandHint || "build latest llama.cpp and copy image to autodiscovered nodes"}</span>
          </div>
        {/if}

        <button class="btn btn--sm" onclick={syncImage} disabled={!!actionRunning || !!imageActionRunning || isBackendActionRunning()}>
          {(actionRunning === "sync_llamacpp_image" || isBackendActionRunning("sync_llamacpp_image"))
            ? runningLabel("sync_llamacpp_image")
            : (syncAction(state)?.label || "Sync Image")}
        </button>
        <div class="text-xs text-txtsecondary break-all">
          Command:
          <span class="font-mono">{syncAction(state)?.commandHint || "docker pull <selected> on autodiscovered nodes + persist as new default"}</span>
        </div>
      </div>
    {:else}
      <div class="text-xs text-txtsecondary">
        La sincronización de imagen no está disponible para el backend actual.
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

  <div class="card flex-1 min-h-0 overflow-auto">
    <div class="text-sm text-txtsecondary mb-2">Imágenes Docker por nodo</div>
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
      <div class="space-y-3">
        {#each nodeImages as node (node.nodeIp)}
          <div class="p-2 border border-card-border rounded bg-background/30">
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
              <div class="space-y-2 max-h-80 overflow-auto pr-1">
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
