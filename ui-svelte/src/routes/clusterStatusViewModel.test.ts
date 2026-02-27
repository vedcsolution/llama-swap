import { describe, expect, it } from "vitest";
import {
  buildClusterVramSummary,
  connectivityProbeLabelForState,
  connectivityStatusLabelForState,
  formatNodeLatency,
} from "./clusterStatusViewModel";
import type { ClusterNodeStatus } from "../lib/types";

describe("clusterStatusViewModel", () => {
  it("formats latency for local, missing and zero values", () => {
    expect(formatNodeLatency(undefined, true)).toBe("local");
    expect(formatNodeLatency(undefined, false)).toBe("-");
    expect(formatNodeLatency(0, false)).toBe("0 ms");
  });

  it("uses connectivity labels based on mode", () => {
    expect(connectivityStatusLabelForState({ connectivityMode: "ssh" })).toBe("SSH OK");
    expect(connectivityStatusLabelForState({ connectivityMode: "agent" })).toBe("Agent OK");
    expect(connectivityProbeLabelForState({ connectivityMode: "ssh" })).toBe("SSH BatchMode");
    expect(connectivityProbeLabelForState({ connectivityMode: "agent" })).toBe("Agent Health");
  });

  it("returns N/A for util_only and count_only VRAM quality", () => {
    const utilOnlyNode: ClusterNodeStatus = {
      ip: "192.168.8.138",
      isLocal: false,
      port22Open: true,
      sshOk: true,
      gpu: {
        queriedAt: "2026-01-01T00:00:00Z",
        quality: "util_only",
        devices: [{ index: 0, utilizationPct: 80, memoryKnown: false, totalMiB: 0, usedMiB: 0, freeMiB: 0 }],
      },
    };
    const countOnlyNode: ClusterNodeStatus = {
      ...utilOnlyNode,
      gpu: {
        queriedAt: "2026-01-01T00:00:00Z",
        quality: "count_only",
        devices: [{ index: 0, memoryKnown: false, totalMiB: 0, usedMiB: 0, freeMiB: 0 }],
      },
    };

    const utilOnlySummary = buildClusterVramSummary(utilOnlyNode);
    expect(utilOnlySummary.label).toContain("N/A");
    expect(utilOnlySummary.note).toContain("Memoria GPU no disponible");

    const countOnlySummary = buildClusterVramSummary(countOnlyNode);
    expect(countOnlySummary.label).toContain("N/A");
    expect(countOnlySummary.note).toContain("Solo conteo de GPU disponible");
  });
});
