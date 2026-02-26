package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

	if got := clusterAutodiscoverPath(); got != scriptPath {
		t.Fatalf("clusterAutodiscoverPath() = %q, want %q", got, scriptPath)
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

func TestProxyManager_ClusterStatusCache_ReusesFreshValue(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "60")
	pm := &ProxyManager{}
	calls := 0
	loader := func(_ context.Context) (clusterStatusState, error) {
		calls++
		return clusterStatusState{DetectedAt: fmt.Sprintf("call-%d", calls)}, nil
	}

	first, err := pm.readClusterStatusCachedWithLoader(context.Background(), false, loader)
	if err != nil {
		t.Fatalf("first readClusterStatusCachedWithLoader error: %v", err)
	}
	second, err := pm.readClusterStatusCachedWithLoader(context.Background(), false, loader)
	if err != nil {
		t.Fatalf("second readClusterStatusCachedWithLoader error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("loader calls = %d, want 1", calls)
	}
	if first.DetectedAt != second.DetectedAt {
		t.Fatalf("cached detectedAt mismatch: first=%q second=%q", first.DetectedAt, second.DetectedAt)
	}
}

func TestProxyManager_ClusterStatusCache_ForceRefreshBypassesCache(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "60")
	pm := &ProxyManager{}
	calls := 0
	loader := func(_ context.Context) (clusterStatusState, error) {
		calls++
		return clusterStatusState{DetectedAt: fmt.Sprintf("call-%d", calls)}, nil
	}

	first, err := pm.readClusterStatusCachedWithLoader(context.Background(), false, loader)
	if err != nil {
		t.Fatalf("first readClusterStatusCachedWithLoader error: %v", err)
	}
	second, err := pm.readClusterStatusCachedWithLoader(context.Background(), true, loader)
	if err != nil {
		t.Fatalf("forced readClusterStatusCachedWithLoader error: %v", err)
	}

	if calls != 2 {
		t.Fatalf("loader calls = %d, want 2", calls)
	}
	if first.DetectedAt == second.DetectedAt {
		t.Fatalf("expected force refresh to replace cache, got same detectedAt=%q", first.DetectedAt)
	}
}

func TestProxyManager_ClusterStatusCache_DisabledWhenTTLZero(t *testing.T) {
	t.Setenv(clusterStatusCacheTTLEnv, "0")
	pm := &ProxyManager{}
	calls := 0
	loader := func(_ context.Context) (clusterStatusState, error) {
		calls++
		return clusterStatusState{DetectedAt: fmt.Sprintf("call-%d", calls)}, nil
	}

	_, err := pm.readClusterStatusCachedWithLoader(context.Background(), false, loader)
	if err != nil {
		t.Fatalf("first readClusterStatusCachedWithLoader error: %v", err)
	}
	_, err = pm.readClusterStatusCachedWithLoader(context.Background(), false, loader)
	if err != nil {
		t.Fatalf("second readClusterStatusCachedWithLoader error: %v", err)
	}

	if calls != 2 {
		t.Fatalf("loader calls = %d, want 2 when cache disabled", calls)
	}
}
