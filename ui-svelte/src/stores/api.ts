import { writable } from "svelte/store";
import type {
  Model,
  Metrics,
  VersionInfo,
  LogData,
  APIEventEnvelope,
  ReqRespCapture,
  BenchyJob,
  BenchyStartResponse,
  BenchyStartOptions,
  RecipeBackendState,
  RecipeBackendAction,
  RecipeBackendActionResponse,
  RecipeBackendActionStatus,
  RecipeBackendHFModelsState,
  RecipeBackendHFRecipeResponse,
  DockerImagesState,
  DockerImageActionResponse,
  RecipeUIState,
  RecipeUpsertRequest,
  RecipeSourceState,
  ConfigEditorState,
  ClusterStatusState,
  ClusterDGXUpdateResponse,
} from "../lib/types";
import { connectionState } from "./theme";

const LOG_LENGTH_LIMIT = 1024 * 100; /* 100KB of log data */

// Stores
export const models = writable<Model[]>([]);
export const proxyLogs = writable<string>("");
export const upstreamLogs = writable<string>("");
export const metrics = writable<Metrics[]>([]);
export const versionInfo = writable<VersionInfo>({
  build_date: "unknown",
  commit: "unknown",
  version: "unknown",
});

let apiEventSource: EventSource | null = null;

function appendLog(newData: string, store: typeof proxyLogs | typeof upstreamLogs): void {
  store.update((prev) => {
    const updatedLog = prev + newData;
    return updatedLog.length > LOG_LENGTH_LIMIT ? updatedLog.slice(-LOG_LENGTH_LIMIT) : updatedLog;
  });
}

export function enableAPIEvents(enabled: boolean): void {
  if (!enabled) {
    apiEventSource?.close();
    apiEventSource = null;
    metrics.set([]);
    return;
  }

  let retryCount = 0;
  const initialDelay = 1000; // 1 second

  const connect = () => {
    apiEventSource?.close();
    apiEventSource = new EventSource("/api/events");

    connectionState.set("connecting");

    apiEventSource.onopen = () => {
      // Clear everything on connect to keep things in sync
      proxyLogs.set("");
      upstreamLogs.set("");
      metrics.set([]);
      retryCount = 0;
      connectionState.set("connected");
    };

    apiEventSource.onmessage = (e: MessageEvent) => {
      try {
        const message = JSON.parse(e.data) as APIEventEnvelope;
        switch (message.type) {
          case "modelStatus": {
            const newModels = JSON.parse(message.data) as Model[];
            // Sort models by name and id
            newModels.sort((a, b) => {
              return (a.name + a.id).localeCompare(b.name + b.id);
            });
            models.set(newModels);
            break;
          }

          case "logData": {
            const logData = JSON.parse(message.data) as LogData;
            switch (logData.source) {
              case "proxy":
                appendLog(logData.data, proxyLogs);
                break;
              case "upstream":
                appendLog(logData.data, upstreamLogs);
                break;
            }
            break;
          }

          case "metrics": {
            const newMetrics = JSON.parse(message.data) as Metrics[];
            metrics.update((prevMetrics) => [...newMetrics, ...prevMetrics]);
            break;
          }
        }
      } catch (err) {
        console.error(e.data, err);
      }
    };

    apiEventSource.onerror = () => {
      apiEventSource?.close();
      retryCount++;
      const delay = Math.min(initialDelay * Math.pow(2, retryCount - 1), 5000);
      connectionState.set("disconnected");
      setTimeout(connect, delay);
    };
  };

  connect();
}

// Fetch version info when connected
connectionState.subscribe(async (status) => {
  if (status === "connected") {
    try {
      const response = await fetch("/api/version");
      if (!response.ok) {
        throw new Error(`HTTP error! status: ${response.status}`);
      }
      const data: VersionInfo = await response.json();
      versionInfo.set(data);
    } catch (error) {
      console.error(error);
    }
  }
});

export async function listModels(): Promise<Model[]> {
  try {
    const response = await fetch("/api/models/");
    if (!response.ok) {
      throw new Error(`HTTP error! status: ${response.status}`);
    }
    const data = await response.json();
    return data || [];
  } catch (error) {
    console.error("Failed to fetch models:", error);
    return [];
  }
}

export async function unloadAllModels(): Promise<void> {
  try {
    const response = await fetch(`/api/models/unload`, {
      method: "POST",
    });
    if (!response.ok) {
      throw new Error(`Failed to unload models: ${response.status}`);
    }
  } catch (error) {
    console.error("Failed to unload models:", error);
    throw error;
  }
}

export interface StopClusterResult {
  message?: string;
  script?: string;
  output?: string;
}

export async function stopCluster(): Promise<StopClusterResult> {
  try {
    const response = await fetch(`/api/cluster/stop`, {
      method: "POST",
    });
    const text = await response.text().catch(() => "");
    let parsed: StopClusterResult | null = null;
    try {
      parsed = text ? (JSON.parse(text) as StopClusterResult) : null;
    } catch {
      parsed = null;
    }
    if (!response.ok) {
      const errMsg = (parsed as any)?.error || text || `Failed to stop cluster: ${response.status}`;
      const output = parsed?.output ? `

${parsed.output}` : "";
      throw new Error(`${errMsg}${output}`);
    }
    return parsed || {};
  } catch (error) {
    console.error("Failed to stop cluster:", error);
    throw error;
  }
}

export async function unloadSingleModel(model: string): Promise<void> {
  try {
    const response = await fetch(`/api/models/unload/${model}`, {
      method: "POST",
    });
    if (!response.ok) {
      throw new Error(`Failed to unload model: ${response.status}`);
    }
  } catch (error) {
    console.error("Failed to unload model", model, error);
    throw error;
  }
}

export async function loadModel(model: string): Promise<void> {
  try {
    const probes = [`/upstream/${model}/v1/models`, `/upstream/${model}/health`, `/upstream/${model}/`];
    let lastStatus = 0;

    for (const url of probes) {
      const response = await fetch(url, { method: "GET" });
      lastStatus = response.status;
      if (response.ok) {
        return;
      }
    }

    throw new Error(`Failed to load model: ${lastStatus}`);
  } catch (error) {
    console.error("Failed to load model:", error);
    throw error;
  }
}

export async function startBenchy(model: string, opts: BenchyStartOptions = {}): Promise<string> {
  const payload = {
    model,
    queueModels: opts.queueModels,
    baseUrl: opts.baseUrl,
    tokenizer: opts.tokenizer,
    pp: opts.pp,
    tg: opts.tg,
    depth: opts.depth,
    concurrency: opts.concurrency,
    runs: opts.runs,
    latencyMode: opts.latencyMode,
    noCache: opts.noCache,
    noWarmup: opts.noWarmup,
    adaptPrompt: opts.adaptPrompt,
    enablePrefixCaching: opts.enablePrefixCaching,
    trustRemoteCode: opts.trustRemoteCode,
    enableIntelligence: opts.enableIntelligence,
    intelligencePlugins: opts.intelligencePlugins,
    allowCodeExec: opts.allowCodeExec,
    datasetCacheDir: opts.datasetCacheDir,
    outputDir: opts.outputDir,
    maxConcurrent: opts.maxConcurrent,
    startAt: opts.startAt,
  };

  const response = await fetch(`/api/benchy`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to start benchy: ${response.status}`);
  }

  const data: BenchyStartResponse = await response.json();
  if (!data?.id) {
    throw new Error("Invalid benchy start response");
  }
  return data.id;
}

export async function getBenchyJob(id: string): Promise<BenchyJob> {
  const response = await fetch(`/api/benchy/${id}`);
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch benchy job: ${response.status}`);
  }
  return (await response.json()) as BenchyJob;
}

export async function cancelBenchyJob(id: string): Promise<void> {
  const response = await fetch(`/api/benchy/${id}/cancel`, {
    method: "POST",
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to cancel benchy job: ${response.status}`);
  }
}

export async function getRecipeUIState(signal?: AbortSignal): Promise<RecipeUIState> {
  const response = await fetch(`/api/recipes/state`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch recipe state: ${response.status}`);
  }
  return (await response.json()) as RecipeUIState;
}

export async function getRecipeBackendState(signal?: AbortSignal): Promise<RecipeBackendState> {
  const response = await fetch(`/api/recipes/backend`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch recipe backend state: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendState;
}

export async function setRecipeBackend(backendDir: string): Promise<RecipeBackendState> {
  const response = await fetch(`/api/recipes/backend`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ backendDir }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to set recipe backend: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendState;
}

export async function getRecipeBackendActionStatus(signal?: AbortSignal): Promise<RecipeBackendActionStatus> {
  const response = await fetch(`/api/recipes/backend/action-status`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch backend action status: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendActionStatus;
}

export async function runRecipeBackendAction(
  action: RecipeBackendAction,
  opts?: { sourceImage?: string; hfModel?: string; hfFormat?: "gguf" | "safetensors"; hfQuantization?: string },
): Promise<RecipeBackendActionResponse> {
  const response = await fetch(`/api/recipes/backend/action`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      action,
      sourceImage: opts?.sourceImage,
      hfModel: opts?.hfModel,
      hfFormat: opts?.hfFormat,
      hfQuantization: opts?.hfQuantization,
    }),
  });

  const responseText = await response.text();
  let parsed: any = null;
  try {
    parsed = responseText ? JSON.parse(responseText) : null;
  } catch {
    parsed = null;
  }

  if (!response.ok) {
    const baseMessage =
      parsed?.error || parsed?.message || responseText || `Failed to run backend action: ${response.status}`;
    const output = parsed?.output ? `\n\n${parsed.output}` : "";
    throw new Error(`${baseMessage}${output}`);
  }

  return (parsed || {}) as RecipeBackendActionResponse;
}

export async function getRecipeBackendHFModels(signal?: AbortSignal): Promise<RecipeBackendHFModelsState> {
  const response = await fetch(`/api/recipes/backend/hf-models`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch HF models: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendHFModelsState;
}

export async function setRecipeBackendHFHubPath(hubPath: string): Promise<RecipeBackendHFModelsState> {
  const response = await fetch(`/api/recipes/backend/hf-models/path`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ hubPath }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to set HF hub path: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendHFModelsState;
}

export async function deleteRecipeBackendHFModel(cacheDir: string): Promise<RecipeBackendHFModelsState> {
  const response = await fetch(`/api/recipes/backend/hf-models`, {
    method: "DELETE",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ cacheDir }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to delete HF model: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendHFModelsState;
}

export async function generateRecipeBackendHFModel(cacheDir: string): Promise<RecipeBackendHFRecipeResponse> {
  const response = await fetch(`/api/recipes/backend/hf-models/recipe`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ cacheDir }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to generate recipe from HF model: ${response.status}`);
  }
  return (await response.json()) as RecipeBackendHFRecipeResponse;
}

export async function upsertRecipeModel(payload: RecipeUpsertRequest): Promise<RecipeUIState> {
  const response = await fetch(`/api/recipes/models`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to save recipe model: ${response.status}`);
  }
  return (await response.json()) as RecipeUIState;
}

export async function deleteRecipeModel(modelId: string): Promise<RecipeUIState> {
  const response = await fetch(`/api/recipes/models/${encodeURIComponent(modelId)}`, {
    method: "DELETE",
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to delete recipe model: ${response.status}`);
  }
  return (await response.json()) as RecipeUIState;
}

export async function getRecipeSourceState(recipeRef: string, signal?: AbortSignal): Promise<RecipeSourceState> {
  const response = await fetch(`/api/recipes/source?recipeRef=${encodeURIComponent(recipeRef)}`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch recipe source: ${response.status}`);
  }
  return (await response.json()) as RecipeSourceState;
}

export async function saveRecipeSourceContent(recipeRef: string, content: string): Promise<RecipeSourceState> {
  const response = await fetch(`/api/recipes/source`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ recipeRef, content }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to save recipe source: ${response.status}`);
  }
  return (await response.json()) as RecipeSourceState;
}

export async function createRecipeSource(
  recipeRef: string,
  content: string,
  overwrite = false,
): Promise<RecipeSourceState> {
  const response = await fetch(`/api/recipes/source/create`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ recipeRef, content, overwrite }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to create recipe source: ${response.status}`);
  }
  return (await response.json()) as RecipeSourceState;
}

export async function getConfigEditorState(signal?: AbortSignal): Promise<ConfigEditorState> {
  const response = await fetch(`/api/config/editor`, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch config editor state: ${response.status}`);
  }
  return (await response.json()) as ConfigEditorState;
}

export async function saveConfigEditorContent(content: string): Promise<ConfigEditorState> {
  const response = await fetch(`/api/config/editor`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ content }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to save config: ${response.status}`);
  }
  return (await response.json()) as ConfigEditorState;
}

export async function getCapture(id: number): Promise<ReqRespCapture | null> {
  try {
    const response = await fetch(`/api/captures/${id}`);
    if (response.status === 404) {
      return null;
    }
    if (!response.ok) {
      throw new Error(`Failed to fetch capture: ${response.status}`);
    }
    return await response.json();
  } catch (error) {
    console.error("Failed to fetch capture:", error);
    return null;
  }
}

export async function getDockerContainers(): Promise<string[]> {
  const response = await fetch("/api/recipes/containers");
  if (!response.ok) {
    throw new Error(`Failed to fetch docker containers: ${response.status}`);
  }
  return await response.json();
}

export async function getDockerImages(forceRefresh = false): Promise<DockerImagesState> {
  const endpoint = forceRefresh ? "/api/images/docker?force=1" : "/api/images/docker";
  const response = await fetch(endpoint);
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch docker images: ${response.status}`);
  }
  return (await response.json()) as DockerImagesState;
}

export async function updateDockerImage(nodeIp: string, reference: string): Promise<DockerImageActionResponse> {
  const response = await fetch("/api/images/docker/update", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ nodeIp, reference }),
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text || `Failed to update docker image: ${response.status}`);
  }
  return (text ? JSON.parse(text) : {}) as DockerImageActionResponse;
}

export async function deleteDockerImage(
  nodeIp: string,
  payload: { id?: string; reference?: string },
): Promise<DockerImageActionResponse> {
  const response = await fetch("/api/images/docker/delete", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ nodeIp, id: payload.id, reference: payload.reference }),
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(text || `Failed to delete docker image: ${response.status}`);
  }
  return (text ? JSON.parse(text) : {}) as DockerImageActionResponse;
}

export async function getSelectedContainer(): Promise<string> {
  const response = await fetch("/api/recipes/selected-container");
  if (!response.ok) {
    throw new Error(`Failed to fetch selected container: ${response.status}`);
  }
  const data = await response.json();
  return data.selectedContainer || "vllm-node:latest";
}

export async function setSelectedContainer(container: string): Promise<string> {
  const response = await fetch("/api/recipes/selected-container", {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ container }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to set selected container: ${response.status}`);
  }
  const data = await response.json();
  return data.selectedContainer;
}

export interface GetClusterStatusOptions {
  signal?: AbortSignal;
  forceRefresh?: boolean;
  view?: "summary" | "full";
  include?: Array<"metrics" | "storage" | "dgx">;
  allowStale?: boolean;
}

export async function getClusterStatus(options: GetClusterStatusOptions = {}): Promise<ClusterStatusState> {
  const {
    signal,
    forceRefresh = false,
    view = "full",
    include,
    allowStale = false,
  } = options;
  const params = new URLSearchParams();
  if (forceRefresh) {
    params.set("force", "1");
  }
  if (view && view !== "full") {
    params.set("view", view);
  }
  if (include && include.length > 0) {
    params.set("include", include.join(","));
  }
  if (allowStale) {
    params.set("allowStale", "1");
  }
  const endpoint = params.size > 0 ? `/api/cluster/status?${params.toString()}` : "/api/cluster/status";
  const response = await fetch(endpoint, { signal });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to fetch cluster status: ${response.status}`);
  }
  return (await response.json()) as ClusterStatusState;
}

export async function runClusterDGXUpdate(targets?: string[]): Promise<ClusterDGXUpdateResponse> {
  const response = await fetch(`/api/cluster/dgx/update`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ targets: targets ?? [] }),
  });
  if (!response.ok) {
    const msg = await response.text().catch(() => "");
    throw new Error(msg || `Failed to execute DGX cluster update: ${response.status}`);
  }
  return (await response.json()) as ClusterDGXUpdateResponse;
}
