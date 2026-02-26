<script lang="ts">
  import {
    models,
    loadModel,
    unloadAllModels,
    stopCluster,
    unloadSingleModel,
    startBenchy,
    getBenchyJob,
    cancelBenchyJob,
    getRecipeSourceState,
    saveRecipeSourceContent,
    createRecipeSource,
    getRecipeUIState,
    upsertRecipeModel,
    deleteRecipeModel,
    getClusterStatus,
  } from "../stores/api";
  import { isNarrow } from "../stores/theme";
  import { persistentStore } from "../stores/persistent";
  import { onMount, onDestroy } from "svelte";
  import BenchyDialog from "./BenchyDialog.svelte";
  import type { BenchyJob, BenchyStartOptions, Model, RecipeManagedModel } from "../lib/types";

  type CodeMirrorModules = {
    basicSetup: typeof import("codemirror").basicSetup;
    yaml: typeof import("@codemirror/lang-yaml").yaml;
    EditorState: typeof import("@codemirror/state").EditorState;
    Compartment: typeof import("@codemirror/state").Compartment;
    EditorView: typeof import("@codemirror/view").EditorView;
    keymap: typeof import("@codemirror/view").keymap;
  };

  let isUnloading = $state(false);
  let isStoppingCluster = $state(false);
  let menuOpen = $state(false);
  let bulkActionError: string | null = $state(null);
  let bulkActionNotice: string | null = $state(null);
  let stopClusterNoticeTimer: ReturnType<typeof setTimeout> | null = null;
  let recipeEditorModelId: string | null = $state(null);
  let recipeEditorRef = $state("");
  let recipeEditorPath = $state("");
  let recipeEditorContent = $state("");
  let recipeEditorOriginal = $state("");
  let recipeEditorUpdatedAt = $state("");
  let recipeEditorLoading = $state(false);
  let recipeEditorSaving = $state(false);
  let recipeEditorError: string | null = $state(null);
  let recipeEditorNotice: string | null = $state(null);
  let recipeEditorController: AbortController | null = null;
  let recipeEditorHost = $state<HTMLDivElement | null>(null);
  let recipeEditorView = $state<any | null>(null);
  let recipeEditorSyncingFromView = false;
  let recipeEditorEditableCompartment: any | null = null;
  let codeMirrorModules: CodeMirrorModules | null = null;
  let codeMirrorModulesPromise: Promise<CodeMirrorModules> | null = null;
  let recipeEditorInitToken = 0;
  let recipeModelsById = $state<Record<string, RecipeManagedModel>>({});
  let clusterNodes = $state<string[]>([]);
  let clusterNodesController: AbortController | null = null;
  let selectedInferenceNodeByModel = $state<Record<string, string>>({});
  let nodeApplyBusyByModel = $state<Record<string, boolean>>({});
  let recipeDeleteBusyByModel = $state<Record<string, boolean>>({});
  let recipeDeleteConfirmModelId = $state<string | null>(null);
  let addRecipeOpen = $state(false);
  let addRecipeBusy = $state(false);
  let addRecipeError: string | null = $state(null);
  let addRecipeNotice: string | null = $state(null);
  let addRecipeRef = $state("");
  let addRecipeModelId = $state("");
  let addRecipeName = $state("");
  let addRecipeDescription = $state("");
  let addRecipeUseModelName = $state("");
  let addRecipeMode = $state<"solo" | "cluster">("solo");
  let addRecipeTensorParallel = $state(1);
  let addRecipeNodes = $state("");
  let addRecipeContainerImage = $state("vllm-node:latest");
  let addRecipeExtraArgs = $state("");
  let addRecipeUnlisted = $state(false);
  let addRecipeYAML = $state("");
  const showUnlistedStore = persistentStore<boolean>("showUnlisted", true);
  const showIdorNameStore = persistentStore<"id" | "name">("showIdorName", "id");

  // Benchy state (single active job in UI)
  let benchyOpen = $state(false);
  let benchyStarting = $state(false);
  let benchyError: string | null = $state(null);
  let benchyJob: BenchyJob | null = $state(null);
  let benchyJobId: string | null = $state(null);
  let benchyModelID: string | null = $state(null);
  let benchyPollTimer: ReturnType<typeof setTimeout> | null = null;

  let benchyBusy = $derived.by(() => {
    return benchyStarting || benchyJob?.status === "running" || benchyJob?.status === "scheduled";
  });

  let filteredModels = $derived.by(() => {
    const filtered = $models.filter((model) => $showUnlistedStore || !model.unlisted);
    const peerModels = filtered.filter((m) => m.peerID);

    const grouped = peerModels.reduce(
      (acc, model) => {
        const peerId = model.peerID || "unknown";
        if (!acc[peerId]) acc[peerId] = [];
        acc[peerId].push(model);
        return acc;
      },
      {} as Record<string, Model[]>
    );

    return {
      regularModels: filtered.filter((m) => !m.peerID),
      peerModelsByPeerId: grouped,
    };
  });

  function recipeModelByID(modelID: string): RecipeManagedModel | undefined {
    return recipeModelsById[modelID];
  }

  function selectNodeFromRecipe(recipe: RecipeManagedModel): string {
    const raw = (recipe.nodes || "").trim();
    if (!raw || raw.includes("${") || raw.includes(",") || raw.includes(" ")) {
      return "";
    }
    return raw;
  }

  function setInferenceNodeSelection(modelID: string, value: string): void {
    selectedInferenceNodeByModel = {
      ...selectedInferenceNodeByModel,
      [modelID]: value,
    };
  }

  function collectRecipeNodes(recipeModels: RecipeManagedModel[]): string[] {
    const nodes = new Set<string>();
    for (const recipeModel of recipeModels) {
      const raw = (recipeModel.nodes || "").trim();
      if (!raw || raw.includes("${")) continue;
      for (const node of raw.split(",")) {
        const value = node.trim();
        if (!value) continue;
        if (value.includes(" ")) continue;
        nodes.add(value);
      }
    }
    return Array.from(nodes);
  }

  function mergeClusterNodes(nextNodes: string[]): void {
    const merged = new Set<string>();
    for (const node of clusterNodes) {
      if (node) merged.add(node);
    }
    for (const node of nextNodes) {
      if (node) merged.add(node);
    }
    clusterNodes = Array.from(merged);
  }

  async function refreshClusterNodesInBackground(timeoutMs = 2500): Promise<void> {
    clusterNodesController?.abort();
    const controller = new AbortController();
    clusterNodesController = controller;
    const timeout = setTimeout(() => controller.abort(), timeoutMs);

    try {
      const next = await getClusterStatus({ signal: controller.signal, view: "summary", allowStale: true });
      if (clusterNodesController !== controller) return;
      mergeClusterNodes((next.nodes || []).map((node) => node.ip).filter((ip) => !!ip));
    } catch (err) {
      if (!controller.signal.aborted) {
        console.error("Failed to fetch cluster nodes", err);
      }
    } finally {
      clearTimeout(timeout);
      if (clusterNodesController === controller) {
        clusterNodesController = null;
      }
    }
  }

  function slugifyRecipeRef(input: string): string {
    return input
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9._-]+/g, "-")
      .replace(/^-+|-+$/g, "");
  }

  function buildAddRecipeTemplate(): string {
    const ref = (addRecipeRef || "new-recipe").trim();
    const model = (addRecipeUseModelName || "org/model").trim();
    const container = (addRecipeContainerImage || "vllm-node:latest").trim();
    const name = (addRecipeName || addRecipeModelId || ref).trim();
    const description = (addRecipeDescription || "Custom recipe created from UI").trim();
    const soloOnly = addRecipeMode === "solo" ? "true" : "false";
    const clusterOnly = addRecipeMode === "cluster" ? "true" : "false";
    const tp = Math.max(1, Number(addRecipeTensorParallel) || 1);

    return [
      'recipe_version: "1"',
      `recipe_ref: ${ref}`,
      `name: ${name}`,
      `description: ${description}`,
      `model: ${model}`,
      `container: ${container}`,
      `solo_only: ${soloOnly}`,
      `cluster_only: ${clusterOnly}`,
      "defaults:",
      `  tensor_parallel: ${tp}`,
      'command: |',
      `  vllm serve ${model} --port {port} --host {host}`,
      "",
    ].join("\n");
  }

  function openAddRecipe(): void {
    addRecipeOpen = true;
    addRecipeBusy = false;
    addRecipeError = null;
    addRecipeNotice = null;
    addRecipeRef = "";
    addRecipeModelId = "";
    addRecipeName = "";
    addRecipeDescription = "";
    addRecipeUseModelName = "";
    addRecipeMode = "solo";
    addRecipeTensorParallel = 1;
    addRecipeNodes = "";
    addRecipeContainerImage = "vllm-node:latest";
    addRecipeExtraArgs = "";
    addRecipeUnlisted = false;
    addRecipeYAML = buildAddRecipeTemplate();
  }

  function closeAddRecipe(): void {
    addRecipeOpen = false;
    addRecipeBusy = false;
    addRecipeError = null;
    addRecipeNotice = null;
  }

  function refreshAddRecipeTemplateFromFields(): void {
    addRecipeYAML = buildAddRecipeTemplate();
  }

  async function submitAddRecipe(): Promise<void> {
    const modelId = (addRecipeModelId || "").trim();
    const recipeRef = slugifyRecipeRef(addRecipeRef || modelId);
    const useModelName = (addRecipeUseModelName || "").trim();

    if (!modelId) {
      addRecipeError = "Model ID is required.";
      addRecipeNotice = null;
      return;
    }
    if (!/^[A-Za-z0-9._-]+$/.test(modelId)) {
      addRecipeError = "Model ID only allows letters, numbers, dot, underscore and dash (no /).";
      addRecipeNotice = null;
      return;
    }
    if (!recipeRef) {
      addRecipeError = "Recipe ref is required.";
      addRecipeNotice = null;
      return;
    }
    if (!useModelName) {
      addRecipeError = "useModelName (HF model) is required.";
      addRecipeNotice = null;
      return;
    }

    addRecipeBusy = true;
    addRecipeError = null;
    addRecipeNotice = null;

    try {
      const recipeYAML = (addRecipeYAML || buildAddRecipeTemplate()).trim();
      let resolvedRecipeRef = recipeRef;
      let createdNow = false;

      try {
        const created = await createRecipeSource(recipeRef, recipeYAML, false);
        resolvedRecipeRef = (created.recipeRef || recipeRef).trim();
        createdNow = true;
      } catch (createErr) {
        const createMsg = createErr instanceof Error ? createErr.message : String(createErr);
        if (!/already exists/i.test(createMsg)) {
          throw createErr;
        }

        const existing = await getRecipeSourceState(recipeRef).catch(() => null);
        resolvedRecipeRef = (existing?.recipeRef || recipeRef).trim();
      }

      const payload: any = {
        modelId,
        recipeRef: resolvedRecipeRef,
        name: (addRecipeName || modelId).trim(),
        description: (addRecipeDescription || "").trim(),
        aliases: [],
        useModelName,
        mode: addRecipeMode,
        tensorParallel: Math.max(1, Number(addRecipeTensorParallel) || 1),
        nodes: addRecipeMode === "cluster" ? (addRecipeNodes || "").trim() : "",
        extraArgs: (addRecipeExtraArgs || "").trim(),
        containerImage: (addRecipeContainerImage || "").trim(),
        group: "managed-recipes",
        unlisted: !!addRecipeUnlisted,
      };

      await upsertRecipeModel(payload);
      await refreshRecipeRuntimeState();
      addRecipeNotice = createdNow
        ? `Recipe ${payload.recipeRef} created and model ${modelId} added.`
        : `Recipe ${payload.recipeRef} already existed; model ${modelId} added.`;
      addRecipeOpen = false;
      bulkActionNotice = addRecipeNotice;
      bulkActionError = null;
    } catch (e) {
      addRecipeError = e instanceof Error ? e.message : String(e);
      addRecipeNotice = null;
    } finally {
      addRecipeBusy = false;
    }
  }

  async function refreshRecipeRuntimeState(): Promise<void> {
    const recipeResult = await Promise.allSettled([getRecipeUIState()]);

    if (recipeResult[0].status === "fulfilled") {
      const byID: Record<string, RecipeManagedModel> = {};
      const selected: Record<string, string> = {};
      const recipeModels = recipeResult[0].value.models || [];
      for (const recipeModel of recipeModels) {
        byID[recipeModel.modelId] = recipeModel;
        selected[recipeModel.modelId] = selectNodeFromRecipe(recipeModel);
      }
      recipeModelsById = byID;
      selectedInferenceNodeByModel = selected;
      clusterNodes = collectRecipeNodes(recipeModels);
    } else {
      console.error("Failed to fetch recipe state", recipeResult[0].reason);
      recipeModelsById = {};
      selectedInferenceNodeByModel = {};
      clusterNodes = [];
    }

    void refreshClusterNodesInBackground();
  }

  function isClusterTensorParallel(recipe: RecipeManagedModel | undefined): boolean {
    if (!recipe) {
      return false;
    }
    return (recipe.tensorParallel || 1) > 1;
  }

  async function handleDeleteRecipe(model: Model): Promise<void> {
    const modelID = (model.id || "").trim();
    if (!modelID) {
      bulkActionError = "Model ID is missing.";
      bulkActionNotice = null;
      return;
    }
    if (recipeDeleteConfirmModelId !== modelID) {
      recipeDeleteConfirmModelId = modelID;
      bulkActionError = null;
      bulkActionNotice = "Confirm delete: click Delete Recipe again for " + modelID + ".";
      return;
    }
    recipeDeleteConfirmModelId = null;

    recipeDeleteBusyByModel = {
      ...recipeDeleteBusyByModel,
      [modelID]: true,
    };
    bulkActionError = null;
    bulkActionNotice = null;

    try {
      await deleteRecipeModel(modelID);
      await refreshRecipeRuntimeState();
      if (recipeEditorModelId === modelID) {
        closeRecipeEditor();
      }
      bulkActionNotice = `Recipe/model ${modelID} removed from config.yaml.`;
    } catch (e) {
      bulkActionError = e instanceof Error ? e.message : String(e);
      bulkActionNotice = null;
    } finally {
      recipeDeleteBusyByModel = {
        ...recipeDeleteBusyByModel,
        [modelID]: false,
      };
    }
  }

  async function applyInferenceNode(model: Model): Promise<void> {
    const recipe = recipeModelByID(model.id);
    if (!recipe) {
      bulkActionError = `No recipe metadata found for model ${model.id}`;
      bulkActionNotice = null;
      return;
    }
    if (isClusterTensorParallel(recipe)) {
      bulkActionError = `Model ${model.id} is cluster TP ${recipe.tensorParallel}; it runs on multiple nodes.`;
      bulkActionNotice = null;
      return;
    }

    const modelID = model.id;
    const selectedNode = (selectedInferenceNodeByModel[modelID] || '').trim();

    nodeApplyBusyByModel = {
      ...nodeApplyBusyByModel,
      [modelID]: true,
    };
    bulkActionError = null;
    bulkActionNotice = null;

    try {
      const payload: any = {
        modelId: recipe.modelId,
        recipeRef: recipe.recipeRef,
        name: recipe.name || '',
        description: recipe.description || '',
        aliases: recipe.aliases || [],
        useModelName: recipe.useModelName || '',
        mode: selectedNode ? 'cluster' : (recipe.mode || 'cluster'),
        tensorParallel: recipe.tensorParallel || 1,
        nodes: selectedNode,
        extraArgs: recipe.extraArgs || '',
        containerImage: recipe.containerImage || '',
        group: recipe.group || 'managed-recipes',
        unlisted: !!recipe.unlisted,
        hotSwap: !!recipe.hotSwap,
        nonPrivileged: !!recipe.nonPrivileged,
        memLimitGb: recipe.memLimitGb || 0,
        memSwapLimitGb: recipe.memSwapLimitGb || 0,
        pidsLimit: recipe.pidsLimit || 0,
        shmSizeGb: recipe.shmSizeGb || 0,
      };
      if (typeof recipe.benchyTrustRemoteCode === 'boolean') {
        payload.benchyTrustRemoteCode = recipe.benchyTrustRemoteCode;
      }

      await upsertRecipeModel(payload);
      await refreshRecipeRuntimeState();

      bulkActionNotice = selectedNode
        ? `Inference node for ${modelID} set to ${selectedNode}.`
        : `Inference node for ${modelID} set to auto (backend default).`;
    } catch (e) {
      bulkActionError = e instanceof Error ? e.message : String(e);
      bulkActionNotice = null;
    } finally {
      nodeApplyBusyByModel = {
        ...nodeApplyBusyByModel,
        [modelID]: false,
      };
    }
  }

  function handleClickOutside(event: MouseEvent) {
    const target = event.target as Element;
    if (!target.closest(".model-container-selector")) {
      document.querySelectorAll(".container-dropdown.open").forEach((dropdown) => {
        dropdown.classList.remove("open");
      });
    }
  }

  onMount(() => {
    document.addEventListener("click", handleClickOutside);
    void refreshRecipeRuntimeState();
  });

  onDestroy(() => {
    document.removeEventListener("click", handleClickOutside);
    clusterNodesController?.abort();
    clusterNodesController = null;
    if (stopClusterNoticeTimer !== null) {
      clearTimeout(stopClusterNoticeTimer);
      stopClusterNoticeTimer = null;
    }
    recipeEditorView?.destroy();
    recipeEditorView = null;
  });

  async function handleUnloadAllModels(): Promise<void> {
    isUnloading = true;
    bulkActionError = null;
    bulkActionNotice = null;
    try {
      await unloadAllModels();
      bulkActionNotice = "Unload All completed (containers kept running).";
    } catch (e) {
      console.error(e);
      bulkActionError = e instanceof Error ? e.message : String(e);
    } finally {
      setTimeout(() => (isUnloading = false), 1000);
    }
  }

  async function handleStopCluster(): Promise<void> {
    isStoppingCluster = true;
    bulkActionError = null;
    bulkActionNotice = null;
    if (stopClusterNoticeTimer !== null) {
      clearTimeout(stopClusterNoticeTimer);
      stopClusterNoticeTimer = null;
    }
    try {
      const result = await stopCluster();
      const summary = (result.message || "Stop Cluster completed").trim();
      const output = (result.output || "").trim();
      bulkActionNotice = output ? `${summary}
${output}` : summary;
      stopClusterNoticeTimer = setTimeout(() => {
        bulkActionNotice = null;
        stopClusterNoticeTimer = null;
      }, 2000);
    } catch (e) {
      console.error(e);
      bulkActionError = e instanceof Error ? e.message : String(e);
    } finally {
      setTimeout(() => (isStoppingCluster = false), 1000);
    }
  }

  function toggleIdorName(): void {
    showIdorNameStore.update((prev) => (prev === "name" ? "id" : "name"));
  }

  function toggleShowUnlisted(): void {
    showUnlistedStore.update((prev) => !prev);
  }
  function getModelDisplay(model: Model): string {
    return $showIdorNameStore === "id" ? model.id : (model.name || model.id);
  }

  function formatRecipeUpdatedAt(value: string): string {
    if (!value) return "unknown";
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return value;
    return parsed.toLocaleString();
  }

  function syncRecipeEditorViewContent(nextContent: string): void {
    if (!recipeEditorView || recipeEditorSyncingFromView) return;
    const currentContent = recipeEditorView.state.doc.toString();
    if (currentContent === nextContent) return;
    recipeEditorView.dispatch({
      changes: {
        from: 0,
        to: currentContent.length,
        insert: nextContent,
      },
    });
  }

  async function loadCodeMirrorModules(): Promise<CodeMirrorModules> {
    if (codeMirrorModules) return codeMirrorModules;
    if (!codeMirrorModulesPromise) {
      codeMirrorModulesPromise = Promise.all([
        import("codemirror"),
        import("@codemirror/lang-yaml"),
        import("@codemirror/state"),
        import("@codemirror/view"),
      ]).then(([codemirrorPkg, yamlPkg, statePkg, viewPkg]) => ({
        basicSetup: codemirrorPkg.basicSetup,
        yaml: yamlPkg.yaml,
        EditorState: statePkg.EditorState,
        Compartment: statePkg.Compartment,
        EditorView: viewPkg.EditorView,
        keymap: viewPkg.keymap,
      }));
    }
    codeMirrorModules = await codeMirrorModulesPromise;
    return codeMirrorModules;
  }

  function closeRecipeEditor(): void {
    recipeEditorController?.abort();
    recipeEditorController = null;
    recipeEditorModelId = null;
    recipeEditorHost = null;
    recipeEditorRef = "";
    recipeEditorPath = "";
    recipeEditorContent = "";
    recipeEditorOriginal = "";
    recipeEditorUpdatedAt = "";
    recipeEditorLoading = false;
    recipeEditorSaving = false;
    recipeEditorError = null;
    recipeEditorNotice = null;
  }

  async function openRecipeEditor(model: Model): Promise<void> {
    const recipeRef = (model.recipeRef || "").trim();
    if (!recipeRef) {
      recipeEditorError = `No recipeRef configured for ${model.id}`;
      recipeEditorNotice = null;
      return;
    }

    recipeEditorController?.abort();
    const controller = new AbortController();
    recipeEditorController = controller;

    recipeEditorModelId = model.id;
    recipeEditorLoading = true;
    recipeEditorSaving = false;
    recipeEditorError = null;
    recipeEditorNotice = null;
    recipeEditorRef = "";
    recipeEditorPath = "";
    recipeEditorContent = "";
    recipeEditorOriginal = "";
    recipeEditorUpdatedAt = "";

    try {
      const state = await getRecipeSourceState(recipeRef, controller.signal);
      recipeEditorRef = state.recipeRef || recipeRef;
      recipeEditorPath = state.path || "";
      recipeEditorContent = state.content || "";
      recipeEditorOriginal = state.content || "";
      recipeEditorUpdatedAt = state.updatedAt || "";
    } catch (e) {
      if (!controller.signal.aborted) {
        recipeEditorError = e instanceof Error ? e.message : String(e);
      }
    } finally {
      if (recipeEditorController === controller) {
        recipeEditorController = null;
      }
      recipeEditorLoading = false;
    }
  }

  async function refreshRecipeEditor(): Promise<void> {
    if (!recipeEditorRef || !recipeEditorModelId) return;
    const model = $models.find((m) => m.id === recipeEditorModelId);
    if (model) {
      await openRecipeEditor(model);
    }
  }

  async function saveRecipeEditor(): Promise<void> {
    if (!recipeEditorRef || recipeEditorSaving || recipeEditorLoading) return;
    if (recipeEditorContent === recipeEditorOriginal) return;

    recipeEditorSaving = true;
    recipeEditorError = null;
    recipeEditorNotice = null;
    try {
      const state = await saveRecipeSourceContent(recipeEditorRef, recipeEditorContent);
      recipeEditorRef = state.recipeRef || recipeEditorRef;
      recipeEditorPath = state.path || recipeEditorPath;
      recipeEditorContent = state.content || recipeEditorContent;
      recipeEditorOriginal = state.content || recipeEditorContent;
      recipeEditorUpdatedAt = state.updatedAt || "";
      recipeEditorNotice = "Recipe YAML guardada correctamente.";
      await refreshRecipeRuntimeState();
    } catch (e) {
      recipeEditorError = e instanceof Error ? e.message : String(e);
    } finally {
      recipeEditorSaving = false;
    }
  }

  function clearBenchyPoll(): void {
    if (benchyPollTimer !== null) {
      clearTimeout(benchyPollTimer);
      benchyPollTimer = null;
    }
  }

  function closeBenchyDialog(): void {
    benchyOpen = false;
    benchyStarting = false;
    benchyError = null;
    benchyModelID = null;
    clearBenchyPoll();
  }

  async function pollBenchy(jobID: string): Promise<void> {
    try {
      const job = await getBenchyJob(jobID);
      benchyJob = job;

      if (job.status === "running" || job.status === "scheduled") {
        benchyPollTimer = setTimeout(() => {
          void pollBenchy(jobID);
        }, 1000);
      } else {
        clearBenchyPoll();
      }
    } catch (e) {
      benchyError = e instanceof Error ? e.message : String(e);
      clearBenchyPoll();
    }
  }

  function waitForModelReady(modelID: string, timeoutMs = 5 * 60 * 1000): Promise<void> {
    // Resolve immediately if already ready
    const current = $models.find((m) => m.id === modelID);
    if (current?.state === "ready") return Promise.resolve();

    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        unsub();
        reject(new Error(`Timed out waiting for ${modelID} to become ready`));
      }, timeoutMs);

      const unsub = models.subscribe((list) => {
        const m = list.find((x) => x.id === modelID);
        if (m?.state === "ready") {
          clearTimeout(timer);
          unsub();
          resolve();
        }
      });
    });
  }

  function openBenchyForModel(modelID: string): void {
    benchyModelID = modelID;
    benchyOpen = true;
    benchyError = null;
  }

  async function runBenchyForModel(opts: BenchyStartOptions): Promise<void> {
    if (!benchyModelID) return;
    const modelID = benchyModelID;

    benchyStarting = true;
    benchyError = null;
    benchyJob = null;
    benchyJobId = null;
    clearBenchyPoll();

    try {
      const m = $models.find((x) => x.id === modelID);
      if (!m) throw new Error(`Model not found: ${modelID}`);
      if (m.state === "stopping" || m.state === "shutdown") {
        throw new Error(`Model is ${m.state}; wait until it is stopped/ready`);
      }

      if (m.state === "stopped") {
        await loadModel(modelID);
      }
      await waitForModelReady(modelID);

      const id = await startBenchy(modelID, opts);
      benchyJobId = id;
      benchyStarting = false;
      await pollBenchy(id);
    } catch (e) {
      benchyStarting = false;
      benchyError = e instanceof Error ? e.message : String(e);
    }
  }

  async function cancelBenchy(): Promise<void> {
    if (!benchyJobId) return;
    try {
      await cancelBenchyJob(benchyJobId);
      await pollBenchy(benchyJobId);
    } catch (e) {
      benchyError = e instanceof Error ? e.message : String(e);
    }
  }

  $effect(() => {
    if (!recipeEditorHost) {
      recipeEditorInitToken += 1;
      recipeEditorView?.destroy();
      recipeEditorView = null;
      recipeEditorEditableCompartment = null;
      return;
    }
    if (recipeEditorView) return;

    const host = recipeEditorHost;
    const token = ++recipeEditorInitToken;
    let cancelled = false;

    void (async () => {
      try {
        const modules = await loadCodeMirrorModules();
        if (cancelled || token !== recipeEditorInitToken) return;
        if (!recipeEditorHost || recipeEditorHost !== host || recipeEditorView) return;

        const { basicSetup, yaml, EditorState, Compartment, EditorView, keymap } = modules;
        recipeEditorEditableCompartment = new Compartment();

        recipeEditorView = new EditorView({
          parent: host,
          state: EditorState.create({
            doc: recipeEditorContent,
            extensions: [
              basicSetup,
              yaml(),
              EditorView.lineWrapping,
              recipeEditorEditableCompartment.of(EditorView.editable.of(!(recipeEditorLoading || recipeEditorSaving))),
              keymap.of([
                {
                  key: "Mod-s",
                  run: () => {
                    void saveRecipeEditor();
                    return true;
                  },
                },
              ]),
              EditorView.updateListener.of((update) => {
                if (!update.docChanged) return;
                recipeEditorSyncingFromView = true;
                recipeEditorContent = update.state.doc.toString();
                recipeEditorSyncingFromView = false;
              }),
              EditorView.theme({
                "&": {
                  height: "100%",
                  fontSize: "13px",
                  fontFamily:
                    '"JetBrains Mono","Fira Code","Cascadia Code",Menlo,Monaco,Consolas,"Liberation Mono","Courier New",monospace',
                  backgroundColor: "transparent",
                },
                "&.cm-focused": {
                  outline: "none",
                },
                ".cm-scroller": {
                  overflow: "auto",
                  lineHeight: "1.5",
                },
                ".cm-content": {
                  padding: "12px 0",
                },
                ".cm-line": {
                  padding: "0 12px",
                },
                ".cm-gutters": {
                  backgroundColor: "rgba(15, 23, 42, 0.35)",
                  borderRight: "1px solid rgba(148, 163, 184, 0.2)",
                },
                ".cm-activeLine": {
                  backgroundColor: "rgba(56, 189, 248, 0.08)",
                },
                ".cm-activeLineGutter": {
                  backgroundColor: "rgba(56, 189, 248, 0.16)",
                },
              }),
            ],
          }),
        });
      } catch (err) {
        if (!cancelled) {
          recipeEditorError = err instanceof Error ? err.message : String(err);
        }
      }
    })();

    return () => {
      cancelled = true;
    };
  });

  $effect(() => {
    syncRecipeEditorViewContent(recipeEditorContent);
  });

  $effect(() => {
    if (!recipeEditorView || !recipeEditorEditableCompartment || !codeMirrorModules) return;
    const { EditorView } = codeMirrorModules;
    recipeEditorView.dispatch({
      effects: recipeEditorEditableCompartment.reconfigure(EditorView.editable.of(!(recipeEditorLoading || recipeEditorSaving))),
    });
  });

</script>

<div class="card h-full flex flex-col">
  <div class="shrink-0">
    <div class="flex justify-between items-baseline">
      <h2 class={$isNarrow ? "text-xl" : ""}>Models</h2>
      {#if $isNarrow}
        <div class="relative">
          <button class="btn text-base flex items-center gap-2 py-1" onclick={() => (menuOpen = !menuOpen)} aria-label="Toggle menu">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
              <path fill-rule="evenodd" d="M3 6.75A.75.75 0 0 1 3.75 6h16.5a.75.75 0 0 1 0 1.5H3.75A.75.75 0 0 1 3 6.75ZM3 12a.75.75 0 0 1 .75-.75h16.5a.75.75 0 0 1 0 1.5H3.75A.75.75 0 0 1 3 12Zm0 5.25a.75.75 0 0 1 .75-.75h16.5a.75.75 0 0 1 0 1.5H3.75a.75.75 0 0 1-.75-.75Z" clip-rule="evenodd" />
            </svg>
          </button>
          {#if menuOpen}
            <div class="absolute right-0 mt-2 w-48 bg-surface border border-gray-200 dark:border-white/10 rounded shadow-lg z-20">
              <button
                class="w-full text-left px-4 py-2 hover:bg-secondary-hover flex items-center gap-2"
                onclick={() => { toggleIdorName(); menuOpen = false; }}
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                  <path fill-rule="evenodd" d="M15.97 2.47a.75.75 0 0 1 1.06 0l4.5 4.5a.75.75 0 0 1 0 1.06l-4.5 4.5a.75.75 0 1 1-1.06-1.06l3.22-3.22H7.5a.75.75 0 0 1 0-1.5h11.69l-3.22-3.22a.75.75 0 0 1 0-1.06Zm-7.94 9a.75.75 0 0 1 0 1.06l-3.22 3.22H16.5a.75.75 0 0 1 0 1.5H4.81l3.22 3.22a.75.75 0 1 1-1.06 1.06l-4.5-4.5a.75.75 0 0 1 0-1.06l4.5-4.5a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
                </svg>
                {$showIdorNameStore === "id" ? "Show Name" : "Show ID"}
              </button>
              <button
                class="w-full text-left px-4 py-2 hover:bg-secondary-hover flex items-center gap-2"
                onclick={() => { toggleShowUnlisted(); menuOpen = false; }}
              >
                {#if $showUnlistedStore}
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                    <path d="M3.53 2.47a.75.75 0 0 0-1.06 1.06l18 18a.75.75 0 1 0 1.06-1.06l-18-18ZM22.676 12.553a11.249 11.249 0 0 1-2.631 4.31l-3.099-3.099a5.25 5.25 0 0 0-6.71-6.71L7.759 4.577a11.217 11.217 0 0 1 4.242-.827c4.97 0 9.185 3.223 10.675 7.69.12.362.12.752 0 1.113Z" />
                    <path d="M15.75 12c0 .18-.013.357-.037.53l-4.244-4.243A3.75 3.75 0 0 1 15.75 12ZM12.53 15.713l-4.243-4.244a3.75 3.75 0 0 0 4.244 4.243Z" />
                    <path d="M6.75 12c0-.619.107-1.213.304-1.764l-3.1-3.1a11.25 11.25 0 0 0-2.63 4.31c-.12.362-.12.752 0 1.114 1.489 4.467 5.704 7.69 10.675 7.69 1.5 0 2.933-.294 4.242-.827l-2.477-2.477A5.25 5.25 0 0 1 6.75 12Z" />
                  </svg>
                {:else}
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                    <path d="M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z" />
                    <path fill-rule="evenodd" d="M1.323 11.447C2.811 6.976 7.028 3.75 12.001 3.75c4.97 0 9.185 3.223 10.675 7.69.12.362.12.752 0 1.113-1.487 4.471-5.705 7.697-10.677 7.697-4.97 0-9.186-3.223-10.675-7.69a1.762 1.762 0 0 1 0-1.113ZM17.25 12a5.25 5.25 0 1 1-10.5 0 5.25 5.25 0 0 1 10.5 0Z" clip-rule="evenodd" />
                  </svg>
                {/if}
                {$showUnlistedStore ? "Hide Unlisted" : "Show Unlisted"}
              </button>
              <button
                class="w-full text-left px-4 py-2 hover:bg-secondary-hover flex items-center gap-2"
                onclick={() => { openAddRecipe(); menuOpen = false; }}
                disabled={addRecipeBusy || isUnloading || isStoppingCluster}
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                  <path fill-rule="evenodd" d="M12 3.75a.75.75 0 0 1 .75.75v6.75h6.75a.75.75 0 0 1 0 1.5h-6.75v6.75a.75.75 0 0 1-1.5 0v-6.75H4.5a.75.75 0 0 1 0-1.5h6.75V4.5a.75.75 0 0 1 .75-.75Z" clip-rule="evenodd" />
                </svg>
                Add Recipe
              </button>
              <button
                class="w-full text-left px-4 py-2 hover:bg-secondary-hover flex items-center gap-2"
                onclick={() => { handleUnloadAllModels(); menuOpen = false; }}
                disabled={isUnloading || isStoppingCluster}
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-6 h-6">
                  <path fill-rule="evenodd" d="M12 2.25c-5.385 0-9.75 4.365-9.75 9.75s4.365 9.75 9.75 9.75 9.75-4.365 9.75-9.75S17.385 2.25 12 2.25Zm.53 5.47a.75.75 0 0 0-1.06 0l-3 3a.75.75 0 1 0 1.06 1.06l1.72-1.72v5.69a.75.75 0 0 0 1.5 0v-5.69l1.72 1.72a.75.75 0 1 0 1.06-1.06l-3-3Z" clip-rule="evenodd" />
                </svg>
                {isUnloading ? "Unloading..." : "Unload All"}
              </button>
              <button
                class="w-full text-left px-4 py-2 hover:bg-secondary-hover flex items-center gap-2"
                onclick={() => { handleStopCluster(); menuOpen = false; }}
                disabled={isStoppingCluster || isUnloading}
              >
                <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-6 h-6">
                  <path fill-rule="evenodd" d="M6.75 6A2.25 2.25 0 0 0 4.5 8.25v7.5A2.25 2.25 0 0 0 6.75 18h10.5a2.25 2.25 0 0 0 2.25-2.25v-7.5A2.25 2.25 0 0 0 17.25 6H6.75Zm2.28 2.22a.75.75 0 0 0-1.06 1.06L10.69 12l-2.72 2.72a.75.75 0 1 0 1.06 1.06L11.75 13.06l2.72 2.72a.75.75 0 1 0 1.06-1.06L12.81 12l2.72-2.72a.75.75 0 1 0-1.06-1.06l-2.72 2.72-2.72-2.72Z" clip-rule="evenodd" />
                </svg>
                {isStoppingCluster ? "Stopping..." : "Stop Cluster"}
              </button>
            </div>
          {/if}
        </div>
      {/if}
    </div>
    {#if !$isNarrow}
      <div class="flex justify-between">
        <div class="flex gap-2">
          <button class="btn text-base flex items-center gap-2" onclick={toggleIdorName} style="line-height: 1.2">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
              <path fill-rule="evenodd" d="M15.97 2.47a.75.75 0 0 1 1.06 0l4.5 4.5a.75.75 0 0 1 0 1.06l-4.5 4.5a.75.75 0 1 1-1.06-1.06l3.22-3.22H7.5a.75.75 0 0 1 0-1.5h11.69l-3.22-3.22a.75.75 0 0 1 0-1.06Zm-7.94 9a.75.75 0 0 1 0 1.06l-3.22 3.22H16.5a.75.75 0 0 1 0 1.5H4.81l3.22 3.22a.75.75 0 1 1-1.06 1.06l-4.5-4.5a.75.75 0 0 1 0-1.06l4.5-4.5a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
            </svg>
            {$showIdorNameStore === "id" ? "ID" : "Name"}
          </button>

          <button class="btn text-base flex items-center gap-2" onclick={toggleShowUnlisted} style="line-height: 1.2">
            {#if $showUnlistedStore}
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                <path d="M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z" />
                <path fill-rule="evenodd" d="M1.323 11.447C2.811 6.976 7.028 3.75 12.001 3.75c4.97 0 9.185 3.223 10.675 7.69.12.362.12.752 0 1.113-1.487 4.471-5.705 7.697-10.677 7.697-4.97 0-9.186-3.223-10.675-7.69a1.762 1.762 0 0 1 0-1.113ZM17.25 12a5.25 5.25 0 1 1-10.5 0 5.25 5.25 0 0 1 10.5 0Z" clip-rule="evenodd" />
              </svg>
            {:else}
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
                <path d="M3.53 2.47a.75.75 0 0 0-1.06 1.06l18 18a.75.75 0 1 0 1.06-1.06l-18-18ZM22.676 12.553a11.249 11.249 0 0 1-2.631 4.31l-3.099-3.099a5.25 5.25 0 0 0-6.71-6.71L7.759 4.577a11.217 11.217 0 0 1 4.242-.827c4.97 0 9.185 3.223 10.675 7.69.12.362.12.752 0 1.113Z" />
                <path d="M15.75 12c0 .18-.013.357-.037.53l-4.244-4.243A3.75 3.75 0 0 1 15.75 12ZM12.53 15.713l-4.243-4.244a3.75 3.75 0 0 0 4.244 4.243Z" />
                <path d="M6.75 12c0-.619.107-1.213.304-1.764l-3.1-3.1a11.25 11.25 0 0 0-2.63 4.31c-.12.362-.12.752 0 1.114 1.489 4.467 5.704 7.69 10.675 7.69 1.5 0 2.933-.294 4.242-.827l-2.477-2.477A5.25 5.25 0 0 1 6.75 12Z" />
              </svg>
            {/if}
            {$showUnlistedStore ? "Hide Unlisted" : "Show Unlisted"}
          </button>

          <button class="btn text-base flex items-center gap-2" onclick={openAddRecipe} disabled={addRecipeBusy || isUnloading || isStoppingCluster}>
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-5 h-5">
              <path fill-rule="evenodd" d="M12 3.75a.75.75 0 0 1 .75.75v6.75h6.75a.75.75 0 0 1 0 1.5h-6.75v6.75a.75.75 0 0 1-1.5 0v-6.75H4.5a.75.75 0 0 1 0-1.5h6.75V4.5a.75.75 0 0 1 .75-.75Z" clip-rule="evenodd" />
            </svg>
            Add Recipe
          </button>
        </div>
        <div class="flex gap-2">
          <button class="btn text-base flex items-center gap-2" onclick={handleStopCluster} disabled={isStoppingCluster || isUnloading}>
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-6 h-6">
              <path fill-rule="evenodd" d="M6.75 6A2.25 2.25 0 0 0 4.5 8.25v7.5A2.25 2.25 0 0 0 6.75 18h10.5a2.25 2.25 0 0 0 2.25-2.25v-7.5A2.25 2.25 0 0 0 17.25 6H6.75Zm2.28 2.22a.75.75 0 0 0-1.06 1.06L10.69 12l-2.72 2.72a.75.75 0 1 0 1.06 1.06L11.75 13.06l2.72 2.72a.75.75 0 1 0 1.06-1.06L12.81 12l2.72-2.72a.75.75 0 1 0-1.06-1.06l-2.72 2.72-2.72-2.72Z" clip-rule="evenodd" />
            </svg>
            {isStoppingCluster ? "Stopping..." : "Stop Cluster"}
          </button>
          <button class="btn text-base flex items-center gap-2" onclick={handleUnloadAllModels} disabled={isUnloading || isStoppingCluster}>
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" class="w-6 h-6">
              <path fill-rule="evenodd" d="M12 2.25c-5.385 0-9.75 4.365-9.75 9.75s4.365 9.75 9.75 9.75 9.75-4.365 9.75-9.75S17.385 2.25 12 2.25Zm.53 5.47a.75.75 0 0 0-1.06 0l-3 3a.75.75 0 1 0 1.06 1.06l1.72-1.72v5.69a.75.75 0 0 0 1.5 0v-5.69l1.72 1.72a.75.75 0 1 0 1.06-1.06l-3-3Z" clip-rule="evenodd" />
            </svg>
            {isUnloading ? "Unloading..." : "Unload All"}
          </button>
        </div>
      </div>
    {/if}
  </div>


  {#if bulkActionError}
    <div class="mt-2 p-2 border border-red-400/30 bg-red-600/10 rounded text-sm text-red-300 break-words">{bulkActionError}</div>
  {/if}
  {#if bulkActionNotice}
    <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 whitespace-pre-wrap break-words">{bulkActionNotice}</div>
  {/if}
  {#if addRecipeError}
    <div class="mt-2 p-2 border border-red-400/30 bg-red-600/10 rounded text-sm text-red-300 break-words">{addRecipeError}</div>
  {/if}
  {#if addRecipeNotice}
    <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words">{addRecipeNotice}</div>
  {/if}

  {#if addRecipeOpen}
    <div class="mt-3 p-3 border border-card-border rounded bg-background/40 space-y-3">
      <div class="flex items-center justify-between gap-2">
        <h3 class="text-base font-semibold">Añadir receta</h3>
        <div class="flex gap-2">
          <button class="btn btn--sm" onclick={refreshAddRecipeTemplateFromFields} disabled={addRecipeBusy}>Regenerate YAML</button>
          <button class="btn btn--sm" onclick={closeAddRecipe} disabled={addRecipeBusy}>Cancel</button>
          <button class="btn btn--sm" onclick={submitAddRecipe} disabled={addRecipeBusy}>{addRecipeBusy ? "Creating..." : "Create"}</button>
        </div>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 gap-2">
        <label class="text-xs text-txtsecondary">Model ID
          <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeModelId} placeholder="Qwen3-Next-Instruct-FP8" />
        </label>
        <label class="text-xs text-txtsecondary">Recipe Ref
          <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeRef} placeholder="qwen3-next-instruct-fp8" />
        </label>
        <label class="text-xs text-txtsecondary">Display Name
          <input class="input mt-1 w-full text-sm" bind:value={addRecipeName} placeholder="Qwen3-Next-Instruct-FP8" />
        </label>
        <label class="text-xs text-txtsecondary">HF Model (useModelName)
          <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeUseModelName} placeholder="Qwen/Qwen3-Next-Instruct-FP8" />
        </label>
        <label class="text-xs text-txtsecondary">Mode
          <select class="mt-1 w-full px-2 py-2 rounded border border-card-border bg-background text-sm" bind:value={addRecipeMode}>
            <option value="solo">solo</option>
            <option value="cluster">cluster</option>
          </select>
        </label>
        <label class="text-xs text-txtsecondary">Tensor Parallel
          <input class="input mt-1 w-full text-sm" type="number" min="1" bind:value={addRecipeTensorParallel} />
        </label>
        <label class="text-xs text-txtsecondary">Inference Backend (Container Image)
          <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeContainerImage} placeholder="vllm-node:latest" />
        </label>
        <label class="text-xs text-txtsecondary">Nodes (cluster only)
          <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeNodes} placeholder="192.168.200.12,192.168.200.13" />
        </label>
      </div>

      <label class="text-xs text-txtsecondary block">Description
        <input class="input mt-1 w-full text-sm" bind:value={addRecipeDescription} placeholder="Recipe description" />
      </label>
      <label class="text-xs text-txtsecondary block">Extra Args
        <input class="input mt-1 w-full font-mono text-sm" bind:value={addRecipeExtraArgs} placeholder="--gpu-memory-utilization 0.9" />
      </label>
      <label class="inline-flex items-center gap-2 text-sm text-txtsecondary">
        <input type="checkbox" bind:checked={addRecipeUnlisted} />
        unlisted
      </label>

      <label class="text-xs text-txtsecondary block">Recipe YAML (saved under ~/swap-laboratories/recipes)
        <textarea class="mt-1 w-full h-56 px-2 py-2 rounded border border-card-border bg-background font-mono text-xs" bind:value={addRecipeYAML}></textarea>
      </label>
    </div>
  {/if}

  <div class="flex-1 overflow-y-auto">
    <table class="w-full">
      <thead class="sticky top-0 bg-card z-10">
        <tr class="text-left border-b border-gray-200 dark:border-white/10 bg-surface">
          <th>{$showIdorNameStore === "id" ? "Model ID" : "Name"}</th>
          <th>Inference Backend</th>
          <th></th>
          <th>State</th>
        </tr>
      </thead>
      <tbody>
        {#each filteredModels.regularModels as model (model.id)}
          <tr class="border-b hover:bg-secondary-hover border-gray-200">
            <td class={model.unlisted ? "text-txtsecondary" : ""}>
              <a href="/upstream/{model.id}/" class="font-semibold" target="_blank">
                {getModelDisplay(model)}
              </a>
              {#if model.description}
                <p class={model.unlisted ? "text-opacity-70" : ""}><em>{model.description}</em></p>
              {/if}
            </td>
            <td class="font-mono text-xs break-all text-txtsecondary w-[28rem] max-w-[28rem]">
              {model.containerImage || "-"}
              {#if recipeModelByID(model.id)}
                {@const recipe = recipeModelByID(model.id)}
                {#if recipe}
                  <div class="mt-2 space-y-1">
                    {#if isClusterTensorParallel(recipe)}
                      <div class="text-[11px] text-txtsecondary">
                        Inference node: cluster TP {recipe.tensorParallel} (usa múltiples nodos)
                      </div>
                    {:else}
                      <div class="text-[11px] text-txtsecondary">Inference node</div>
                      <div class="flex items-center gap-2">
                        <select
                          class="px-2 py-1 rounded border border-card-border bg-background text-xs font-mono"
                          value={selectedInferenceNodeByModel[model.id] || ""}
                          onchange={(event) =>
                            setInferenceNodeSelection(model.id, (event.currentTarget as HTMLSelectElement).value)}
                          disabled={!!nodeApplyBusyByModel[model.id]}
                        >
                          <option value="">auto (backend default)</option>
                          {#each clusterNodes as nodeIP}
                            <option value={nodeIP}>{nodeIP}</option>
                          {/each}
                        </select>
                        <button class="btn btn--sm" onclick={() => applyInferenceNode(model)} disabled={!!nodeApplyBusyByModel[model.id]}>
                          {nodeApplyBusyByModel[model.id] ? "Saving..." : "Apply Node"}
                        </button>
                      </div>
                    {/if}
                  </div>
                {/if}
              {/if}
            </td>
            <td class="w-auto">
              <div class="flex justify-end gap-2 items-center flex-wrap">
                {#if model.state === "stopped"}
                  <button class="btn btn--sm" onclick={() => loadModel(model.id)} disabled={benchyBusy}>Load</button>
                {:else}
                  <button class="btn btn--sm" onclick={() => unloadSingleModel(model.id)} disabled={model.state !== "ready" || benchyBusy}>Unload</button>
                {/if}

                <button
                  class="btn btn--sm"
                  onclick={() => openBenchyForModel(model.id)}
                  disabled={benchyBusy || model.state === "stopping" || model.state === "shutdown" || model.state === "unknown"}
                  title={model.state === "stopped" ? "Load + configure llama-benchy" : "Configure llama-benchy"}
                >
                  Benchy
                </button>

                <button
                  class="btn btn--sm"
                  onclick={() => openRecipeEditor(model)}
                  disabled={recipeEditorLoading || recipeEditorSaving || !model.recipeRef}
                  title={model.recipeRef ? "Edit recipe YAML" : "Model has no recipeRef"}
                >
                  Edit Recipe
                </button>

                <button
                  class="btn btn--sm"
                  onclick={() => handleDeleteRecipe(model)}
                  disabled={!!recipeDeleteBusyByModel[model.id] || (recipeEditorSaving && recipeEditorModelId === model.id)}
                  title="Delete managed recipe/model from config.yaml"
                >
                  {recipeDeleteBusyByModel[model.id] ? "Deleting..." : (recipeDeleteConfirmModelId === model.id ? "Confirm Delete" : "Delete Recipe")}
                </button>
              </div>
            </td>
            <td class="w-20">
              <span class="w-16 text-center status status--{model.state}">{model.state}</span>
            </td>
          </tr>

          {#if recipeEditorModelId === model.id}
            <tr class="border-b border-gray-200">
              <td colspan="4" class="p-3">
                <div class="rounded border border-card-border bg-background/40 p-3">
                  <div class="flex flex-wrap items-center justify-between gap-2">
                    <div class="text-xs text-txtsecondary break-all">
                      Recipe: <span class="font-mono">{recipeEditorRef || "-"}</span>
                      {#if recipeEditorPath}
                        | File: <span class="font-mono">{recipeEditorPath}</span>
                      {/if}
                      {#if recipeEditorUpdatedAt}
                        | Updated: {formatRecipeUpdatedAt(recipeEditorUpdatedAt)}
                      {/if}
                    </div>
                    <div class="flex gap-2">
                      <button class="btn btn--sm" onclick={refreshRecipeEditor} disabled={recipeEditorLoading || recipeEditorSaving}>Refresh</button>
                      <button class="btn btn--sm" onclick={saveRecipeEditor} disabled={recipeEditorLoading || recipeEditorSaving || !recipeEditorRef || recipeEditorContent === recipeEditorOriginal}>
                        {recipeEditorSaving ? "Saving..." : "Save"}
                      </button>
                      <button class="btn btn--sm" onclick={closeRecipeEditor} disabled={recipeEditorSaving}>Close</button>
                    </div>
                  </div>

                  {#if recipeEditorError}
                    <div class="mt-2 p-2 border border-red-400/30 bg-red-600/10 rounded text-sm text-red-300 break-words">{recipeEditorError}</div>
                  {/if}
                  {#if recipeEditorNotice}
                    <div class="mt-2 p-2 border border-green-400/30 bg-green-600/10 rounded text-sm text-green-300 break-words">{recipeEditorNotice}</div>
                  {/if}

                  <div class="relative mt-2 w-full min-h-[280px] rounded border border-card-border bg-background overflow-hidden">
                    <div bind:this={recipeEditorHost} class="h-full w-full min-h-[280px]"></div>
                    {#if recipeEditorLoading}
                      <div class="absolute inset-0 grid place-items-center bg-background/80 text-xs text-txtsecondary">
                        Loading recipe source...
                      </div>
                    {/if}
                  </div>
                </div>
              </td>
            </tr>
          {/if}
        {/each}

        {#if filteredModels.regularModels.length === 0 && Object.keys(filteredModels.peerModelsByPeerId).length === 0}
          <tr class="border-b border-gray-200 dark:border-white/10">
            <td colspan="4" class="py-6 text-center text-sm text-txtsecondary">
              {$models.length === 0
                ? "No hay modelos cargados."
                : ($showUnlistedStore
                    ? "No hay modelos visibles."
                    : "No hay modelos visibles con el filtro actual (unlisted ocultos).")}
            </td>
          </tr>
        {/if}
      </tbody>
    </table>

    {#if Object.keys(filteredModels.peerModelsByPeerId).length > 0}
      <h3 class="mt-8 mb-2">Peer Models</h3>
      {#each Object.entries(filteredModels.peerModelsByPeerId).sort(([a], [b]) => a.localeCompare(b)) as [peerId, peerModels] (peerId)}
        <div class="mb-4">
          <table class="w-full">
            <thead class="sticky top-0 bg-card z-10">
              <tr class="text-left border-b border-gray-200 dark:border-white/10 bg-surface">
                <th class="font-semibold">{peerId}</th>
              </tr>
            </thead>
            <tbody>
              {#each peerModels as model (model.id)}
                <tr class="border-b hover:bg-secondary-hover border-gray-200">
                  <td class="pl-8 {model.unlisted ? 'text-txtsecondary' : ''}">
                    <span>{model.id}</span>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/each}
    {/if}
  </div>
</div>

<BenchyDialog
  model={benchyModelID}
  job={benchyJob}
  open={benchyOpen}
  canStart={!!benchyModelID && !benchyBusy}
  starting={benchyStarting}
  error={benchyError}
  onstart={runBenchyForModel}
  onclose={closeBenchyDialog}
  oncancel={cancelBenchy}
/>

<style>
  .model-container-selector {
    position: relative;
    display: inline-block;
  }

  .btn-container-selector {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 0.375rem 0.5rem;
    background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    border: 1px solid #5a67d8;
    border-radius: 0.375rem;
    color: white;
    cursor: pointer;
    transition: all 0.2s ease;
    min-width: 2rem;
  }

  .btn-container-selector:hover {
    background: linear-gradient(135deg, #5a67d8 0%, #6b46c1 100%);
    border-color: #4c51bf;
    transform: scale(1.05);
    box-shadow: 0 4px 6px rgba(0, 0, 0, 0.2);
  }

  .btn-container-selector.just-selected {
    background: linear-gradient(135deg, #48bb78 0%, #38a169 100%);
    border-color: #2f855a;
    animation: pulse 0.5s ease-in-out;
  }

  @keyframes pulse {
    0%, 100% { transform: scale(1); }
    50% { transform: scale(1.1); }
  }

  .container-dropdown {
    position: absolute;
    top: calc(100% + 0.25rem);
    right: 0;
    z-index: 50;
    min-width: 16rem;
    background: white;
    border: 1px solid #e2e8f0;
    border-radius: 0.5rem;
    box-shadow: 0 10px 15px -3px rgba(0, 0, 0, 0.1), 0 4px 6px -2px rgba(0, 0, 0, 0.05);
    opacity: 0;
    visibility: hidden;
    transform: translateY(-0.5rem);
    transition: all 0.2s ease;
  }

  .container-dropdown.open {
    opacity: 1;
    visibility: visible;
    transform: translateY(0);
  }

  .container-dropdown-header {
    padding: 0.75rem 1rem;
    border-bottom: 1px solid #e2e8f0;
    background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    color: white;
    font-weight: 600;
    font-size: 0.875rem;
    border-radius: 0.5rem 0.5rem 0 0;
  }

  .container-dropdown-items {
    max-height: 16rem;
    overflow-y: auto;
    padding: 0.5rem;
  }

  .container-dropdown-item {
    padding: 0.625rem 0.75rem;
    cursor: pointer;
    border-radius: 0.375rem;
    transition: all 0.2s ease;
    border: 1px solid transparent;
  }

  .container-dropdown-item:hover {
    background: linear-gradient(135deg, #f7fafc 0%, #edf2f7 100%);
    border-color: #cbd5e0;
    transform: translateX(0.125rem);
  }

  .container-tag {
    display: block;
    font-family: 'Courier New', monospace;
    font-size: 0.75rem;
    font-weight: 600;
    color: #2d3748;
    padding: 0.25rem 0.5rem;
    background: linear-gradient(135deg, #edf2f7 0%, #e2e8f0 100%);
    border-radius: 0.25rem;
    border: 1px solid #cbd5e0;
  }

  /* Dark mode support */
  :global([data-theme="dark"]) .container-dropdown {
    background: #2d3748;
    border-color: #4a5568;
  }

  :global([data-theme="dark"]) .container-dropdown-header {
    background: linear-gradient(135deg, #553c9a 0%, #44337a 100%);
  }

  :global([data-theme="dark"]) .container-dropdown-item:hover {
    background: linear-gradient(135deg, #4a5568 0%, #2d3748 100%);
    border-color: #718096;
  }

  :global([data-theme="dark"]) .container-tag {
    color: #e2e8f0;
    background: linear-gradient(135deg, #4a5568 0%, #2d3748 100%);
    border-color: #718096;
  }
</style>
