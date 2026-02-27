import type { ClusterNodeStatus, ClusterStatusState } from "../lib/types";

export type NodeMetricSummary = {
  percent: number | null;
  label: string;
  error?: string;
  note?: string;
};

export function formatNodeLatency(value: number | undefined, isLocal: boolean): string {
  if (isLocal) return "local";
  if (value == null || value < 0) return "-";
  return `${value} ms`;
}

export function formatCacheAgeMs(value?: number): string {
  if (value == null || value < 0) return "-";
  if (value < 1000) return `${value} ms`;
  return `${(value / 1000).toFixed(1)} s`;
}

export function formatDurationMsLabel(value?: number): string {
  if (value == null || value < 0) return "-";
  if (value < 1000) return `${value} ms`;
  return `${(value / 1000).toFixed(1)} s`;
}

export function connectivityStatusLabelForState(nextState: Pick<ClusterStatusState, "connectivityMode"> | null | undefined): string {
  return nextState?.connectivityMode === "agent" ? "Agent OK" : "SSH OK";
}

export function connectivityProbeLabelForState(nextState: Pick<ClusterStatusState, "connectivityMode"> | null | undefined): string {
  return nextState?.connectivityMode === "agent" ? "Agent Health" : "SSH BatchMode";
}

export function formatMiB(value?: number): string {
  if (value == null || value < 0) return "-";
  const gib = value / 1024;
  if (gib >= 100) return `${Math.round(gib)} GiB`;
  if (gib >= 10) return `${gib.toFixed(1)} GiB`;
  return `${gib.toFixed(2)} GiB`;
}

export function buildClusterVramSummary(node: ClusterNodeStatus): NodeMetricSummary {
  if (node.gpu?.error) {
    return { percent: null, label: "-", error: node.gpu.error };
  }
  const devices = node.gpu?.devices || [];
  if (devices.length === 0) {
    return { percent: null, label: "sin GPU" };
  }

  const quality = node.gpu?.quality;
  if (quality === "count_only") {
    return { percent: null, label: `N/A (${devices.length} GPU)`, note: "Solo conteo de GPU disponible." };
  }
  if (quality === "util_only") {
    return { percent: null, label: `N/A (${devices.length} GPU)`, note: "Memoria GPU no disponible." };
  }

  const memoryKnownDevices = devices.filter((device) => device.memoryKnown !== false);
  if (memoryKnownDevices.length === 0) {
    return { percent: null, label: `N/A (${devices.length} GPU)`, note: "Memoria GPU no disponible." };
  }

  const totalMiB = memoryKnownDevices.reduce((sum, device) => sum + (device.totalMiB || 0), 0);
  const usedMiB = memoryKnownDevices.reduce((sum, device) => sum + (device.usedMiB || 0), 0);
  if (totalMiB <= 0) {
    return { percent: null, label: `N/A (${devices.length} GPU)`, note: "Memoria GPU no disponible." };
  }
  const usage = Math.round((usedMiB / totalMiB) * 100);
  return { percent: usage, label: `${usage}% (${formatMiB(usedMiB)} / ${formatMiB(totalMiB)})` };
}
