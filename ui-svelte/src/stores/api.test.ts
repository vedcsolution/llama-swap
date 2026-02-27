import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getClusterStatus } from "./api";

describe("getClusterStatus", () => {
  const fetchMock = vi.fn();

  beforeEach(() => {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ nodes: [] }),
    });
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("uses the default cluster status endpoint", async () => {
    await getClusterStatus();
    expect(fetchMock).toHaveBeenCalledWith("/api/cluster/status", { signal: undefined });
  });

  it("builds query parameters for progressive loading options", async () => {
    await getClusterStatus({
      forceRefresh: true,
      view: "summary",
      include: ["metrics", "storage"],
      allowStale: true,
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/cluster/status?force=1&view=summary&include=metrics%2Cstorage&allowStale=1",
      { signal: undefined }
    );
  });

  it("keeps additive cluster metadata fields from backend payload", async () => {
    fetchMock.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        nodes: [],
        execMode: "agent",
        connectivityMode: "agent",
        cacheState: "fresh",
        cacheAgeMs: 12,
        timingsMs: { autodiscover: 1, probe: 2, metrics: 3, storage: 4, dgx: 5, total: 6 },
      }),
    });

    const result = await getClusterStatus();
    expect(result.execMode).toBe("agent");
    expect(result.connectivityMode).toBe("agent");
    expect(result.cacheState).toBe("fresh");
    expect(result.cacheAgeMs).toBe(12);
    expect(result.timingsMs?.total).toBe(6);
  });
});
