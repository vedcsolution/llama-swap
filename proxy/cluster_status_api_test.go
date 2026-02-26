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
