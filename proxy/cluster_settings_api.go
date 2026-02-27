package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	clusterSettingsOverrideFileEnv = "LLAMA_SWAP_CLUSTER_SETTINGS_FILE"
)

type clusterSettingsPayload struct {
	ExecMode      string `json:"execMode,omitempty"`
	InventoryFile string `json:"inventoryFile,omitempty"`
}

type clusterSettingsWizardPayload struct {
	Nodes          string `json:"nodes"`
	HeadNode       string `json:"headNode,omitempty"`
	EthIF          string `json:"ethIf,omitempty"`
	IbIF           string `json:"ibIf,omitempty"`
	DefaultSSHUser string `json:"defaultSshUser,omitempty"`
	InventoryFile  string `json:"inventoryFile,omitempty"`
}

type clusterSettingsState struct {
	ExecMode                   string `json:"execMode"`
	RequestedExecMode          string `json:"requestedExecMode,omitempty"`
	InventoryFile              string `json:"inventoryFile,omitempty"`
	AutoDetectedInventoryFile  string `json:"autoDetectedInventoryFile,omitempty"`
	InventoryExists            bool   `json:"inventoryExists"`
	ClusterExecModeEnvironment string `json:"clusterExecModeEnv,omitempty"`
}

func (pm *ProxyManager) apiGetClusterSettings(c *gin.Context) {
	c.JSON(http.StatusOK, pm.currentClusterSettingsState())
}

func (pm *ProxyManager) apiSetClusterSettings(c *gin.Context) {
	var req clusterSettingsPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.ExecMode))
	switch mode {
	case "", "auto":
		mode = ""
	case clusterExecModeLocal, clusterExecModeAgent:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "execMode must be one of: auto, local, agent"})
		return
	}
	inventory := expandLeadingTilde(strings.TrimSpace(req.InventoryFile))
	if inventory != "" {
		if _, err := os.Stat(inventory); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("inventoryFile not found: %s", inventory)})
			return
		}
	}

	override := clusterRuntimeOverride{
		ExecMode:      mode,
		InventoryFile: inventory,
	}
	setClusterRuntimeOverride(override)
	if err := pm.persistClusterRuntimeOverride(override); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist cluster settings failed: " + err.Error()})
		return
	}
	pm.invalidateClusterStatusCache()
	dockerImagesCache.mu.Lock()
	dockerImagesCache.valid = false
	dockerImagesCache.mu.Unlock()

	c.JSON(http.StatusOK, pm.currentClusterSettingsState())
}

func (pm *ProxyManager) apiWizardClusterSettings(c *gin.Context) {
	var req clusterSettingsWizardPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return
	}

	nodes := parseWizardNodes(req.Nodes)
	if len(nodes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nodes is required (comma/newline separated IP or hostname list)"})
		return
	}
	head := strings.TrimSpace(req.HeadNode)
	if head == "" {
		head = nodes[0]
	}
	if !containsString(nodes, head) {
		nodes = append([]string{head}, nodes...)
	}
	nodes = uniqueNonEmptyStrings(nodes)

	inventoryPath := expandLeadingTilde(strings.TrimSpace(req.InventoryFile))
	if inventoryPath == "" {
		inventoryPath = pm.defaultClusterInventoryPath()
	}
	inventoryPath = filepath.Clean(inventoryPath)

	body := buildWizardInventoryYAML(nodes, head, strings.TrimSpace(req.EthIF), strings.TrimSpace(req.IbIF), strings.TrimSpace(req.DefaultSSHUser))
	parent := filepath.Dir(inventoryPath)
	if parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create inventory directory failed: " + err.Error()})
			return
		}
	}
	if err := os.WriteFile(inventoryPath, []byte(body), 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write inventory failed: " + err.Error()})
		return
	}

	override := clusterRuntimeOverride{
		ExecMode:      clusterExecModeAgent,
		InventoryFile: inventoryPath,
	}
	setClusterRuntimeOverride(override)
	if err := pm.persistClusterRuntimeOverride(override); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist cluster settings failed: " + err.Error()})
		return
	}
	pm.invalidateClusterStatusCache()
	dockerImagesCache.mu.Lock()
	dockerImagesCache.valid = false
	dockerImagesCache.mu.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"settings": pm.currentClusterSettingsState(),
		"wizard": gin.H{
			"inventoryFile": inventoryPath,
			"nodes":         nodes,
			"headNode":      head,
		},
	})
}

func (pm *ProxyManager) currentClusterSettingsState() clusterSettingsState {
	override := clusterRuntimeOverrideSnapshot()
	execModeEnv := strings.TrimSpace(os.Getenv(clusterExecModeEnv))
	execMode := clusterExecMode()
	inventory := clusterInventoryFilePath()
	autoDetected := ""
	if override.InventoryFile == "" && strings.TrimSpace(os.Getenv(clusterInventoryFileEnv)) == "" {
		autoDetected = clusterInventoryAutoDetectPath()
	}
	_, err := os.Stat(inventory)
	exists := err == nil
	return clusterSettingsState{
		ExecMode:                   execMode,
		RequestedExecMode:          override.ExecMode,
		InventoryFile:              inventory,
		AutoDetectedInventoryFile:  autoDetected,
		InventoryExists:            exists,
		ClusterExecModeEnvironment: execModeEnv,
	}
}

func (pm *ProxyManager) defaultClusterInventoryPath() string {
	current := strings.TrimSpace(clusterInventoryFilePath())
	if current != "" {
		return current
	}
	if pm != nil && strings.TrimSpace(pm.configPath) != "" {
		return filepath.Join(filepath.Dir(pm.configPath), "cluster-inventory.yaml")
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "cluster-inventory.yaml")
	}
	return "cluster-inventory.yaml"
}

func parseWizardNodes(raw string) []string {
	raw = strings.ReplaceAll(raw, "\n", ",")
	raw = strings.ReplaceAll(raw, "\r", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func buildWizardInventoryYAML(nodes []string, head, ethIF, ibIF, defaultSSHUser string) string {
	var b strings.Builder
	b.WriteString("version: 1\n\n")
	b.WriteString("rdma:\n")
	if strings.TrimSpace(ethIF) != "" || strings.TrimSpace(ibIF) != "" {
		b.WriteString("  required: true\n")
	} else {
		b.WriteString("  required: false\n")
	}
	if strings.TrimSpace(ethIF) != "" {
		b.WriteString(fmt.Sprintf("  eth_if: %s\n", strings.TrimSpace(ethIF)))
	}
	if strings.TrimSpace(ibIF) != "" {
		b.WriteString(fmt.Sprintf("  ib_if: %s\n", strings.TrimSpace(ibIF)))
	}
	b.WriteString("\nagent:\n")
	b.WriteString(fmt.Sprintf("  default_port: %d\n\n", clusterAgentDefaultPort))
	b.WriteString("nodes:\n")
	for idx, node := range nodes {
		id := fmt.Sprintf("node-%d", idx+1)
		if node == head {
			id = "head"
		}
		b.WriteString(fmt.Sprintf("  - id: %s\n", id))
		if node == head {
			b.WriteString("    head: true\n")
		}
		b.WriteString(fmt.Sprintf("    data_ip: %s\n", node))
		b.WriteString(fmt.Sprintf("    control_ip: %s\n", node))
		b.WriteString(fmt.Sprintf("    proxy_ip: %s\n", node))
		if strings.TrimSpace(defaultSSHUser) != "" {
			b.WriteString(fmt.Sprintf("    ssh_user: %s\n", strings.TrimSpace(defaultSSHUser)))
		}
	}
	return b.String()
}

func (pm *ProxyManager) clusterSettingsOverrideFile() string {
	return pm.resolveOverrideFilePath(clusterSettingsOverrideFileEnv, ".cluster_settings.json")
}

func (pm *ProxyManager) loadClusterRuntimeOverride() {
	filePath := pm.clusterSettingsOverrideFile()
	if filePath == "" {
		return
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var override clusterRuntimeOverride
	if err := json.Unmarshal(raw, &override); err != nil {
		return
	}
	override.InventoryFile = expandLeadingTilde(strings.TrimSpace(override.InventoryFile))
	setClusterRuntimeOverride(override)
}

func (pm *ProxyManager) persistClusterRuntimeOverride(override clusterRuntimeOverride) error {
	filePath := pm.clusterSettingsOverrideFile()
	if filePath == "" {
		return nil
	}
	override.InventoryFile = strings.TrimSpace(override.InventoryFile)
	override.ExecMode = strings.ToLower(strings.TrimSpace(override.ExecMode))
	if override.ExecMode == "auto" {
		override.ExecMode = ""
	}
	if override.ExecMode != clusterExecModeLocal && override.ExecMode != clusterExecModeAgent {
		override.ExecMode = ""
	}

	if override.ExecMode == "" && override.InventoryFile == "" {
		if err := os.Remove(filePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}

	parent := filepath.Dir(filePath)
	if parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}

	payload, err := json.MarshalIndent(override, "", "  ")
	if err != nil {
		return err
	}
	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}

func (pm *ProxyManager) invalidateClusterStatusCache() {
	pm.clusterStatusCacheMu.Lock()
	pm.clusterStatusCacheEntries = make(map[string]clusterStatusCacheEntry)
	pm.clusterStatusCacheRefreshInFlight = make(map[string]bool)
	pm.clusterStatusCacheMu.Unlock()
}
