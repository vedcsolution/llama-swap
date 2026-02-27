package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestProxyManager_ClusterStatus_ResponseIncludesMetaFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	pm := &ProxyManager{}
	router.GET("/api/cluster/status", pm.apiGetClusterStatus)

	now := time.Now()
	pm.clusterStatusCacheEntries = map[string]clusterStatusCacheEntry{
		string(clusterStatusViewSummary): {
			State: clusterStatusState{
				AutodiscoverPath: "/tmp/autodiscover.sh",
				DetectedAt:       now.UTC().Format(time.RFC3339),
				LocalIP:          "192.168.8.121",
				CIDR:             "192.168.8.121/24",
				EthIF:            "eth0",
				IbIF:             "ib0",
				NodeCount:        2,
				RemoteCount:      1,
				ReachableBySSH:   2,
				Overall:          "healthy",
				Summary:          "Cluster OK",
				Nodes: []clusterNodeStatus{
					{IP: "192.168.8.121", IsLocal: true, Port22Open: true, SSHOK: true},
					{IP: "192.168.8.138", IsLocal: false, Port22Open: true, SSHOK: true, SSHLatency: clusterInt64Ptr(7)},
				},
			},
			Timings: clusterStatusTimings{
				Autodiscover: 11 * time.Millisecond,
				Probe:        22 * time.Millisecond,
				Total:        33 * time.Millisecond,
			},
			CachedAt:  now,
			ExpiresAt: now.Add(time.Minute),
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/status?view=summary", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if got, ok := payload["execMode"].(string); !ok || got != "local" {
		t.Fatalf("execMode = %v, want local", payload["execMode"])
	}
	if got, ok := payload["connectivityMode"].(string); !ok || got != "ssh" {
		t.Fatalf("connectivityMode = %v, want ssh", payload["connectivityMode"])
	}
	if got, ok := payload["cacheState"].(string); !ok || got != "fresh" {
		t.Fatalf("cacheState = %v, want fresh", payload["cacheState"])
	}
	if _, ok := payload["cacheAgeMs"]; !ok {
		t.Fatalf("cacheAgeMs missing in payload: %v", payload)
	}
	timingsRaw, ok := payload["timingsMs"].(map[string]any)
	if !ok {
		t.Fatalf("timingsMs missing or invalid: %v", payload["timingsMs"])
	}
	for _, key := range []string{"autodiscover", "probe", "metrics", "storage", "dgx", "total"} {
		if _, ok := timingsRaw[key]; !ok {
			t.Fatalf("timingsMs.%s missing: %v", key, timingsRaw)
		}
	}
	if _, ok := payload["summary"].(string); !ok {
		t.Fatalf("summary missing from payload: %v", payload)
	}
}

func TestProxyManager_ClusterStatus_ConnectivityMode_LocalVsAgent(t *testing.T) {
	testCases := []struct {
		name         string
		execMode     string
		connectivity string
	}{
		{
			name:         "local",
			execMode:     "local",
			connectivity: "ssh",
		},
		{
			name:         "agent",
			execMode:     "agent",
			connectivity: "agent",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(clusterExecModeEnv, tc.execMode)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			pm := &ProxyManager{}
			router.GET("/api/cluster/status", pm.apiGetClusterStatus)
			now := time.Now()
			pm.clusterStatusCacheEntries = map[string]clusterStatusCacheEntry{
				string(clusterStatusViewSummary): {
					State: clusterStatusState{
						DetectedAt: now.UTC().Format(time.RFC3339),
						Overall:    "healthy",
						Summary:    "ok",
						Nodes:      []clusterNodeStatus{{IP: "127.0.0.1", IsLocal: true, Port22Open: true, SSHOK: true}},
					},
					CachedAt:  now,
					ExpiresAt: now.Add(time.Minute),
				},
			}

			req := httptest.NewRequest(http.MethodGet, "/api/cluster/status?view=summary", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode json: %v", err)
			}
			if got := payload["connectivityMode"]; got != tc.connectivity {
				t.Fatalf("connectivityMode = %v, want %s", got, tc.connectivity)
			}
		})
	}
}

func TestProxyManager_ClusterStatus_Port22Latency_ZeroIsSerialized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	pm := &ProxyManager{}
	router.GET("/api/cluster/status", pm.apiGetClusterStatus)

	now := time.Now()
	pm.clusterStatusCacheEntries = map[string]clusterStatusCacheEntry{
		string(clusterStatusViewSummary): {
			State: clusterStatusState{
				DetectedAt: now.UTC().Format(time.RFC3339),
				Overall:    "healthy",
				Summary:    "ok",
				Nodes: []clusterNodeStatus{
					{
						IP:            "192.168.8.138",
						IsLocal:       false,
						Port22Open:    true,
						Port22Latency: clusterInt64Ptr(0),
						SSHOK:         true,
						SSHLatency:    clusterInt64Ptr(0),
					},
				},
			},
			CachedAt:  now,
			ExpiresAt: now.Add(time.Minute),
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/status?view=summary", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	nodes, ok := payload["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("nodes missing or invalid: %v", payload["nodes"])
	}
	node, ok := nodes[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid node payload: %v", nodes[0])
	}
	if got, ok := node["port22LatencyMs"]; !ok || got != float64(0) {
		t.Fatalf("port22LatencyMs = %v, want 0", node["port22LatencyMs"])
	}
	if got, ok := node["sshLatencyMs"]; !ok || got != float64(0) {
		t.Fatalf("sshLatencyMs = %v, want 0", node["sshLatencyMs"])
	}
}

func TestProxyManager_ClusterStatus_GPUQuality_UtilOnlyWhenMemoryUnknown(t *testing.T) {
	temp := t.TempDir()
	scriptPath := filepath.Join(temp, "nvidia-smi")
	script := `#!/bin/bash
if [[ "$1" == "--query-gpu=utilization.gpu,memory.total,memory.used,memory.free" ]]; then
  echo "87, N/A, N/A, N/A"
  exit 0
fi
if [[ "$1" == "-L" ]]; then
  echo "GPU 0: Fake GPU"
  exit 0
fi
echo "unexpected args: $*" >&2
exit 1
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nvidia-smi: %v", err)
	}
	t.Setenv("PATH", temp+string(os.PathListSeparator)+os.Getenv("PATH"))

	devices, quality, err := queryNodeGPUMemory(context.Background(), "127.0.0.1", true)
	if err != nil {
		t.Fatalf("queryNodeGPUMemory error: %v", err)
	}
	if quality != "util_only" {
		t.Fatalf("quality = %q, want util_only", quality)
	}
	if len(devices) != 1 {
		t.Fatalf("devices len = %d, want 1", len(devices))
	}
	if devices[0].UtilizationPct == nil || *devices[0].UtilizationPct != 87 {
		t.Fatalf("utilizationPct = %v, want 87", devices[0].UtilizationPct)
	}
	if devices[0].MemoryKnown == nil || *devices[0].MemoryKnown {
		t.Fatalf("memoryKnown = %v, want false", devices[0].MemoryKnown)
	}
}

func TestClusterAutodiscoverPath_PrefersEnv(t *testing.T) {
	temp := t.TempDir()
	envPath := filepath.Join(temp, "custom-autodiscover.sh")
	t.Setenv(clusterAutodiscoverPathEnv, envPath)

	if got := clusterAutodiscoverPath(); got != envPath {
		t.Fatalf("clusterAutodiscoverPath() = %q, want %q", got, envPath)
	}
}

func TestClusterAutodiscoverPath_UsesWorkingDirectory(t *testing.T) {
	temp := t.TempDir()
	scriptPath := filepath.Join(temp, "autodiscover.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("write autodiscover.sh: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(temp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})
	t.Setenv(clusterAutodiscoverPathEnv, "")

	got := normalizePathForComparison(t, clusterAutodiscoverPath())
	want := normalizePathForComparison(t, scriptPath)
	if got != want {
		t.Fatalf("clusterAutodiscoverPath() = %q, want %q", got, want)
	}
}

func TestAPIGetClusterStatus_ErrorPayloadOmitsBackendDir(t *testing.T) {
	t.Setenv(clusterAutodiscoverPathEnv, filepath.Join(t.TempDir(), "missing-autodiscover.sh"))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	pm := &ProxyManager{}
	router.GET("/api/cluster/status", pm.apiGetClusterStatus)

	req := httptest.NewRequest(http.MethodGet, "/api/cluster/status", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode json: %v", err)
	}

	if _, ok := payload["backendDir"]; ok {
		t.Fatalf("backendDir should be omitted from error payload, got=%v", payload["backendDir"])
	}
	if _, ok := payload["autodiscoverPath"]; !ok {
		t.Fatalf("autodiscoverPath missing in error payload: %v", payload)
	}
}

func TestProxyManager_ClusterStatusSummary_SkipsHeavyCollectors(t *testing.T) {
	opts := parseOptsForURL(t, "/api/cluster/status?view=summary")
	if opts.View != clusterStatusViewSummary {
		t.Fatalf("view = %q, want %q", opts.View, clusterStatusViewSummary)
	}
	if opts.Include.Metrics || opts.Include.Storage || opts.Include.DGX {
		t.Fatalf("summary view should disable heavy collectors, got include=%+v", opts.Include)
	}
}

func TestProxyManager_ClusterStatusFull_DefaultIncludesAll(t *testing.T) {
	opts := parseOptsForURL(t, "/api/cluster/status")
	if opts.View != clusterStatusViewFull {
		t.Fatalf("view = %q, want %q", opts.View, clusterStatusViewFull)
	}
	if !opts.Include.Metrics || !opts.Include.Storage || !opts.Include.DGX {
		t.Fatalf("default full view should include all collectors, got include=%+v", opts.Include)
	}
}

func TestProxyManager_ClusterStatusIncludeMask_SelectsCollectors(t *testing.T) {
	opts := parseOptsForURL(t, "/api/cluster/status?view=full&include=metrics,dgx")
	if !opts.Include.Metrics || opts.Include.Storage || !opts.Include.DGX {
		t.Fatalf("unexpected include mask: %+v", opts.Include)
	}
}

func TestAPIGetClusterStatus_BackwardCompatibleWithoutQueryParams(t *testing.T) {
	opts := parseOptsForURL(t, "/api/cluster/status")
	if opts.ForceRefresh {
		t.Fatalf("force refresh should be disabled by default")
	}
	if opts.AllowStale {
		t.Fatalf("allowStale should be disabled by default")
	}
	if opts.View != clusterStatusViewFull {
		t.Fatalf("default view = %q, want %q", opts.View, clusterStatusViewFull)
	}
	if !opts.Include.Metrics || !opts.Include.Storage || !opts.Include.DGX {
		t.Fatalf("default include should keep compatibility with full payload, got include=%+v", opts.Include)
	}
}

func TestProxyManager_ClusterStatusCache_AllowStaleReturnsImmediately(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "60")
	pm := &ProxyManager{}
	includeAll := clusterStatusIncludeSet{Metrics: true, Storage: true, DGX: true}
	opts := clusterStatusRequestOptions{
		View:       clusterStatusViewFull,
		Include:    includeAll,
		AllowStale: true,
	}
	key := opts.cacheKey()
	now := time.Now()
	pm.clusterStatusCacheEntries = map[string]clusterStatusCacheEntry{
		key: {
			State:     clusterStatusState{DetectedAt: "stale"},
			Timings:   clusterStatusTimings{Total: 7 * time.Second},
			CachedAt:  now.Add(-2 * time.Minute),
			ExpiresAt: now.Add(-time.Minute),
		},
	}
	pm.clusterStatusCacheRefreshInFlight = make(map[string]bool)

	loaderCalled := make(chan struct{}, 1)
	loader := func(_ context.Context, _ clusterStatusLoadOptions) (clusterStatusState, clusterStatusTimings, error) {
		select {
		case loaderCalled <- struct{}{}:
		default:
		}
		time.Sleep(120 * time.Millisecond)
		return clusterStatusState{DetectedAt: "fresh"}, clusterStatusTimings{Total: 123 * time.Millisecond}, nil
	}

	startedAt := time.Now()
	result, err := pm.readClusterStatusCachedWithLoader(context.Background(), opts, loader)
	if err != nil {
		t.Fatalf("readClusterStatusCachedWithLoader error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 80*time.Millisecond {
		t.Fatalf("stale response should be immediate, got elapsed=%s", elapsed)
	}
	if result.CacheState != "stale" {
		t.Fatalf("cache state = %q, want stale", result.CacheState)
	}
	if result.State.DetectedAt != "stale" {
		t.Fatalf("detectedAt = %q, want stale payload", result.State.DetectedAt)
	}

	select {
	case <-loaderCalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected background refresh loader to run")
	}

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		pm.clusterStatusCacheMu.Lock()
		entry := pm.clusterStatusCacheEntries[key]
		pm.clusterStatusCacheMu.Unlock()
		if entry.State.DetectedAt == "fresh" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected stale cache entry to be refreshed in background")
}

func TestProxyManager_ClusterStatusCache_ForceRefreshBypassesStale(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "60")
	pm := &ProxyManager{}
	includeAll := clusterStatusIncludeSet{Metrics: true, Storage: true, DGX: true}
	opts := clusterStatusRequestOptions{
		View:         clusterStatusViewFull,
		Include:      includeAll,
		AllowStale:   true,
		ForceRefresh: true,
	}
	key := opts.cacheKey()
	now := time.Now()
	pm.clusterStatusCacheEntries = map[string]clusterStatusCacheEntry{
		key: {
			State:     clusterStatusState{DetectedAt: "stale"},
			Timings:   clusterStatusTimings{Total: 5 * time.Second},
			CachedAt:  now.Add(-2 * time.Minute),
			ExpiresAt: now.Add(-time.Minute),
		},
	}
	pm.clusterStatusCacheRefreshInFlight = make(map[string]bool)

	var calls int32
	loader := func(_ context.Context, _ clusterStatusLoadOptions) (clusterStatusState, clusterStatusTimings, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond)
		return clusterStatusState{DetectedAt: "fresh"}, clusterStatusTimings{Total: 100 * time.Millisecond}, nil
	}

	startedAt := time.Now()
	result, err := pm.readClusterStatusCachedWithLoader(context.Background(), opts, loader)
	if err != nil {
		t.Fatalf("readClusterStatusCachedWithLoader error: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 90*time.Millisecond {
		t.Fatalf("force refresh should bypass stale cache, elapsed=%s", elapsed)
	}
	if result.CacheState != "miss" {
		t.Fatalf("cache state = %q, want miss", result.CacheState)
	}
	if result.State.DetectedAt != "fresh" {
		t.Fatalf("detectedAt = %q, want fresh payload", result.State.DetectedAt)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("loader calls = %d, want 1", got)
	}
}

func TestProxyManager_ClusterStatusCache_DisabledWhenTTLZero(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "0")
	pm := &ProxyManager{}
	calls := 0
	loader := func(_ context.Context, _ clusterStatusLoadOptions) (clusterStatusState, clusterStatusTimings, error) {
		calls++
		return clusterStatusState{DetectedAt: fmt.Sprintf("call-%d", calls)}, clusterStatusTimings{}, nil
	}

	_, err := pm.readClusterStatusCachedWithLoader(context.Background(), clusterStatusRequestOptions{
		View:    clusterStatusViewFull,
		Include: clusterStatusIncludeSet{Metrics: true, Storage: true, DGX: true},
	}, loader)
	if err != nil {
		t.Fatalf("first readClusterStatusCachedWithLoader error: %v", err)
	}
	_, err = pm.readClusterStatusCachedWithLoader(context.Background(), clusterStatusRequestOptions{
		View:    clusterStatusViewFull,
		Include: clusterStatusIncludeSet{Metrics: true, Storage: true, DGX: true},
	}, loader)
	if err != nil {
		t.Fatalf("second readClusterStatusCachedWithLoader error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("loader calls = %d, want 2 when cache disabled", calls)
	}
}

func parseOptsForURL(t *testing.T, rawURL string) clusterStatusRequestOptions {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, rawURL, nil)
	return parseClusterStatusRequestOptions(ctx)
}

func normalizePathForComparison(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolved)
}
