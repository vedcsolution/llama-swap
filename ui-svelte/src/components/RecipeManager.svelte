<script lang="ts">
  import { onMount } from "svelte";
  import {
    deleteRecipeModel,
    getDockerContainers,
    getRecipeUIState,
    upsertRecipeModel,
  } from "../stores/api";
  import type { RecipeCatalogItem, RecipeManagedModel, RecipeUIState } from "../lib/types";
  import { collapseHomePath } from "../lib/pathDisplay";

  let loading = true;
  let saving = false;
  let error: string | null = null;
  let notice: string | null = null;

  let state: RecipeUIState | null = null;
  let selectedModelID = "";

  let modelId = "";
  let recipeRef = "";
  let name = "";
  let description = "";
  let aliasesCsv = "";
  let useModelName = "";
  let mode: "solo" | "cluster" = "cluster";
  let tensorParallel = 2;
  let nodes = "";
  let extraArgs = "";
  let group = "managed-recipes";
  let unlisted = false;
  let benchyTrustRemoteCode: "auto" | "true" | "false" = "auto";
  let hotSwap = false;
  let containerImage = "";
  let availableContainers: string[] = [];
  let refreshController: AbortController | null = null;

  function clearForm(): void {
    selectedModelID = "";
    modelId = "";
    recipeRef = "";
    name = "";
    description = "";
    aliasesCsv = "";
    useModelName = "";
    mode = "cluster";
    tensorParallel = 2;
    nodes = "";
    hotSwap = false;
    containerImage = "";
    extraArgs = "";
    group = "managed-recipes";
    unlisted = false;
    benchyTrustRemoteCode = "auto";
  }

  function loadModelIntoForm(model: RecipeManagedModel): void {
    selectedModelID = model.modelId;
    modelId = model.modelId;
    recipeRef = model.recipeRef || "";
    name = model.name || "";
    description = model.description || "";
    aliasesCsv = (model.aliases || []).join(", ");
    useModelName = model.useModelName || "";
    mode = model.mode || "cluster";
    tensorParallel = model.tensorParallel || 1;
    nodes = model.nodes || "";
    extraArgs = model.extraArgs || "";
    group = model.group || "managed-recipes";
    unlisted = !!model.unlisted;
    if (model.benchyTrustRemoteCode === true) {
      benchyTrustRemoteCode = "true";
    } else if (model.benchyTrustRemoteCode === false) {
      benchyTrustRemoteCode = "false";
    } else {
      benchyTrustRemoteCode = "auto";
    }

    containerImage = model.containerImage || model.metadata?.container_image || "";
  }

  async function refreshState(): Promise<void> {
    refreshController?.abort();
    const controller = new AbortController();
    refreshController = controller;
    const timeout = setTimeout(() => controller.abort(), 10000);

    void loadDockerContainers();
    loading = true;
    error = null;
    try {
      state = await getRecipeUIState(controller.signal);
      if (state.groups.length > 0 && !state.groups.includes(group)) {
        group = state.groups[0];
      }
    } catch (e) {
      if (controller.signal.aborted) {
        error = "Timeout al cargar recetas. Pulsa Refresh para reintentar.";
      } else {
        error = e instanceof Error ? e.message : String(e);
      }
    } finally {
      clearTimeout(timeout);
      if (refreshController === controller) {
        refreshController = null;
      }
      loading = false;
    }
  }

  async function loadDockerContainers(): Promise<void> {
    try {
      availableContainers = await getDockerContainers();
    } catch (e) {
      console.error("Failed to load containers:", e);
      availableContainers = [];
    }
  }

  function parseAliases(raw: string): string[] {
    return raw
      .split(",")
      .map((a) => a.trim())
      .filter(Boolean);
  }

  function applyRecipeDefaults(recipe: RecipeCatalogItem): void {
    recipeRef = recipe.ref || recipe.id;
    if (!name.trim()) {
      name = recipe.name || "";
    }
    if (!description.trim()) {
      description = recipe.description || "";
    }
    if (recipe.soloOnly) {
      mode = "solo";
    } else if (recipe.clusterOnly) {
      mode = "cluster";
    }
    if (recipe.defaultTensorParallel > 0) {
      tensorParallel = recipe.defaultTensorParallel;
    }
    if (!extraArgs.trim() && recipe.defaultExtraArgs) {
      extraArgs = recipe.defaultExtraArgs;
    }
    if (recipe.containerImage) {
      containerImage = recipe.containerImage;
    }
    notice = `Receta seleccionada: ${recipe.id}`;
  }

  async function save(): Promise<void> {
    const id = modelId.trim();
    const recipe = recipeRef.trim();
    if (!id) {
      error = "modelId es obligatorio";
      return;
    }
    if (!recipe) {
      error = "recipeRef es obligatorio";
      return;
    }

    saving = true;
    error = null;
    notice = null;
    try {
      const payload: any = {
        modelId: id,
        recipeRef: recipe,
        name: name.trim(),
        description: description.trim(),
        aliases: parseAliases(aliasesCsv),
        useModelName: useModelName.trim(),
        mode,
        tensorParallel,
        nodes: nodes.trim(),
        extraArgs: extraArgs.trim(),
        containerImage: containerImage.trim(),
        group: group.trim(),
        unlisted,
        hotSwap,
      };
      if (benchyTrustRemoteCode === "true") {
        payload.benchyTrustRemoteCode = true;
      } else if (benchyTrustRemoteCode === "false") {
        payload.benchyTrustRemoteCode = false;
      }

      state = await upsertRecipeModel(payload);
      notice = hotSwap
        ? "Modelo cambiado sin reiniciar cluster (hot swap)."
        : "Guardado. Swap Laboratories regeneró config.yaml automáticamente.";
      selectedModelID = id;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }

  async function removeSelected(): Promise<void> {
    const id = (selectedModelID || modelId).trim();
    if (!id) {
      return;
    }
    saving = true;
    error = null;
    notice = null;
    try {
      state = await deleteRecipeModel(id);
      notice = `Modelo ${id} eliminado y config.yaml actualizado.`;
      clearForm();
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      saving = false;
    }
  }

  onMount(() => {
    void refreshState();
    void loadDockerContainers();
    return () => {
      refreshController?.abort();
    };
  });
</script>

<div class="card mt-4">
  <div class="flex items-center justify-between">
    <h3>Recipe Manager</h3>
    <button class="btn btn--sm" onclick={refreshState} disabled={loading || saving}>Refresh</button>
  </div>

  {#if loading}
    <div class="text-sm text-txtsecondary">Loading recipe state...</div>
  {:else}
    <div class="text-xs text-txtsecondary mb-3 break-all">
      Config:
      <span class="font-mono" title={state?.configPath || ""}>{collapseHomePath(state?.configPath || "")}</span>
      |
      Recipes:
      <span class="font-mono" title={state?.backendDir || ""}>{collapseHomePath(state?.backendDir || "")}</span>
    </div>

    {#if error}
      <div class="p-2 border border-error/40 bg-error/10 rounded text-sm text-error break-words mb-2">{error}</div>
    {/if}
    {#if notice}
      <div class="p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words mb-2">{notice}</div>
    {/if}

    <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Model ID</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={modelId} placeholder="Qwen/Qwen3-Coder-Next-FP8" />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Recipe Ref</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={recipeRef} list="recipes-list" placeholder="qwen3-coder-next-fp8 o /ruta/recipe.yaml" />
        <datalist id="recipes-list">
          {#each state?.recipes || [] as r}
            <option value={r.ref}>{r.id} - {r.name || r.model}</option>
          {/each}
        </datalist>
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Name</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={name} placeholder="Display name" />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Description</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={description} placeholder="Optional description" />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Aliases (comma separated)</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={aliasesCsv} placeholder="alias1, alias2" />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">useModelName</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={useModelName} placeholder="HF model id served by vLLM" />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Mode</div>
        <select class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={mode}>
          <option value="cluster">cluster</option>
          <option value="solo">solo</option>
        </select>
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Tensor Parallel (--tp)</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono" type="number" min="1" bind:value={tensorParallel} />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Nodes (-n)</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono" bind:value={nodes} placeholder={'${vllm_nodes}'} disabled={mode === "solo"} />
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Group</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={group} list="groups-list" placeholder="managed-recipes" />
        <datalist id="groups-list">
          {#each state?.groups || [] as g}
            <option value={g}></option>
          {/each}
        </datalist>
      </label>
      <label class="text-sm md:col-span-2">
        <div class="text-txtsecondary mb-1">Extra Args</div>
        <input class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono" bind:value={extraArgs} placeholder="--gpu-mem 0.9 --max-model-len 185000 -- --enable-prefix-caching" />
      </label>
      <label class="text-sm md:col-span-2">
        <div class="text-txtsecondary mb-1">Inference Backend (Container Image)</div>
        <div class="text-xs text-txtsecondary mb-1">Este contenedor se usará para inferencia de este modelo</div>
        <input
          class="w-full px-2 py-1 rounded border border-card-border bg-background font-mono"
          bind:value={containerImage}
          list="container-images-list"
          placeholder="vllm-node:latest"
        />
        <datalist id="container-images-list">
          {#each availableContainers as container}
            <option value={container}></option>
          {/each}
        </datalist>
      </label>
      <label class="text-sm">
        <div class="text-txtsecondary mb-1">Benchy trust_remote_code</div>
        <select class="w-full px-2 py-1 rounded border border-card-border bg-background" bind:value={benchyTrustRemoteCode}>
          <option value="auto">auto</option>
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      </label>
      <label class="text-sm flex items-center gap-2 pt-6">
        <input type="checkbox" bind:checked={unlisted} />
        unlisted
      </label>
      <label class="text-sm flex items-center gap-2 pt-2" title="No matar el cluster Ray al cambiar modelo (solo disponible en cluster mode)">
        <input type="checkbox" bind:checked={hotSwap} />
        <span>Hot Swap (no matar cluster)</span>
      </label>
    </div>

    <div class="flex gap-2 mt-3">
      <button class="btn btn--sm" onclick={save} disabled={saving}>{selectedModelID ? "Update" : "Add"}</button>
      <button class="btn btn--sm" onclick={removeSelected} disabled={saving || (!selectedModelID && !modelId.trim())}>Delete</button>
      <button class="btn btn--sm" onclick={clearForm} disabled={saving}>New</button>
    </div>

    <div class="mt-4">
      <h4 class="mb-1">Available Recipes ({state?.recipes.length || 0})</h4>
      <div class="overflow-x-auto border border-card-border rounded max-h-72">
        <table class="w-full text-sm">
          <thead class="bg-surface text-left sticky top-0">
            <tr>
              <th class="px-2 py-1">Recipe ID</th>
              <th class="px-2 py-1">Name</th>
              <th class="px-2 py-1">Model</th>
              <th class="px-2 py-1">Mode</th>
              <th class="px-2 py-1">TP</th>
              <th class="px-2 py-1">Inference Backend</th>
              <th class="px-2 py-1">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#if (state?.recipes.length || 0) === 0}
              <tr><td class="px-2 py-2 text-txtsecondary" colspan="7">No recipes found for this backend filter.</td></tr>
            {:else}
              {#each state?.recipes || [] as r (r.id)}
                <tr class="border-t border-card-border">
                  <td class="px-2 py-1 font-mono">{r.id}</td>
                  <td class="px-2 py-1">{r.name || "-"}</td>
                  <td class="px-2 py-1 font-mono text-xs break-all">{r.model || "-"}</td>
                  <td class="px-2 py-1">{r.soloOnly ? "solo" : r.clusterOnly ? "cluster" : "both"}</td>
                  <td class="px-2 py-1">{r.defaultTensorParallel || 1}</td>
                  <td class="px-2 py-1 font-mono text-xs break-all">{r.containerImage || "-"}</td>
                  <td class="px-2 py-1">
                    <button class="btn btn--sm" onclick={() => applyRecipeDefaults(r)} disabled={saving}>Use</button>
                  </td>
                </tr>
              {/each}
            {/if}
          </tbody>
        </table>
      </div>
    </div>

    <div class="mt-4">
      <h4 class="mb-1">Managed Models</h4>
      <div class="overflow-x-auto border border-card-border rounded">
        <table class="w-full text-sm">
          <thead class="bg-surface text-left">
            <tr>
              <th class="px-2 py-1">Model ID</th>
              <th class="px-2 py-1">Recipe</th>
              <th class="px-2 py-1">Mode</th>
              <th class="px-2 py-1">TP</th>
              <th class="px-2 py-1">Group</th>
              <th class="px-2 py-1">Inference Backend</th>
              <th class="px-2 py-1">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#if (state?.models.length || 0) === 0}
              <tr><td class="px-2 py-2 text-txtsecondary" colspan="7">No recipe models yet.</td></tr>
            {:else}
              {#each state?.models || [] as m (m.modelId)}
                <tr class="border-t border-card-border">
                  <td class="px-2 py-1 font-mono">{m.modelId}</td>
                  <td class="px-2 py-1 font-mono">{m.recipeRef}</td>
                  <td class="px-2 py-1">{m.mode}</td>
                  <td class="px-2 py-1">{m.tensorParallel || 1}</td>
                  <td class="px-2 py-1">{m.group}</td>
                  <td class="px-2 py-1 font-mono text-xs break-all">{m.containerImage || "-"}</td>
                  <td class="px-2 py-1">
                    <div class="flex gap-1">
                      <button class="btn btn--sm" onclick={() => loadModelIntoForm(m)} disabled={saving}>Edit</button>
                    </div>
                  </td>
                </tr>
              {/each}
            {/if}
          </tbody>
        </table>
      </div>
    </div>
  {/if}
</div>
