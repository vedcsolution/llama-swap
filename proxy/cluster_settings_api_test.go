package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProxyManager_ClusterSettings_SetAndGet(t *testing.T) {
	reset := clusterRuntimeOverrideSnapshot()
	t.Cleanup(func() {
		setClusterRuntimeOverride(reset)
	})
	setClusterRuntimeOverride(clusterRuntimeOverride{})

	temp := t.TempDir()
	inventoryPath := filepath.Join(temp, "inventory.yaml")
	if err := os.WriteFile(inventoryPath, []byte("version: 1\nnodes:\n  - data_ip: 127.0.0.1\n    control_ip: 127.0.0.1\n"), 0o644); err != nil {
		t.Fatalf("write inventory: %v", err)
	}

	pm := &ProxyManager{
		configPath: filepath.Join(temp, "config.yaml"),
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PUT("/api/cluster/settings", pm.apiSetClusterSettings)
	router.GET("/api/cluster/settings", pm.apiGetClusterSettings)

	body := map[string]any{
		"execMode":      "agent",
		"inventoryFile": inventoryPath,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/cluster/settings", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/cluster/settings", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getRec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["execMode"]; got != "agent" {
		t.Fatalf("execMode = %v, want agent", got)
	}
	if got := payload["inventoryExists"]; got != true {
		t.Fatalf("inventoryExists = %v, want true", got)
	}
}

func TestProxyManager_ClusterSettings_WizardWritesInventory(t *testing.T) {
	reset := clusterRuntimeOverrideSnapshot()
	t.Cleanup(func() {
		setClusterRuntimeOverride(reset)
	})
	setClusterRuntimeOverride(clusterRuntimeOverride{})

	temp := t.TempDir()
	pm := &ProxyManager{
		configPath: filepath.Join(temp, "config.yaml"),
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/cluster/settings/wizard", pm.apiWizardClusterSettings)

	reqBody := map[string]any{
		"nodes":          "192.168.8.121\n192.168.8.138",
		"headNode":       "192.168.8.121",
		"ethIf":          "enp1s0f1np1",
		"ibIf":           "rocep1s0f1,roceP2p1s0f1",
		"defaultSshUser": "csolutions_ai",
	}
	raw, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/api/cluster/settings/wizard", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("wizard status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode wizard response: %v", err)
	}
	settings, ok := payload["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings missing: %v", payload)
	}
	if got := settings["execMode"]; got != "agent" {
		t.Fatalf("execMode = %v, want agent", got)
	}
	wizard, ok := payload["wizard"].(map[string]any)
	if !ok {
		t.Fatalf("wizard missing: %v", payload)
	}
	inventoryFile, _ := wizard["inventoryFile"].(string)
	if inventoryFile == "" {
		t.Fatalf("wizard inventoryFile missing: %v", wizard)
	}
	if _, err := os.Stat(inventoryFile); err != nil {
		t.Fatalf("wizard inventory file not written: %v", err)
	}
}
