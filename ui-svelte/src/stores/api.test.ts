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
});
