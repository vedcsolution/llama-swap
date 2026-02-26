package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mostlygeek/llama-swap/proxy/config"
	"gopkg.in/yaml.v3"
)

const (
	recipesBackendDirEnv            = "LLAMA_SWAP_RECIPES_BACKEND_DIR"
	recipesCatalogDirEnv            = "LLAMA_SWAP_RECIPES_DIR"
	recipesBackendOverrideFileEnv   = "LLAMA_SWAP_RECIPES_BACKEND_OVERRIDE_FILE"
	hfDownloadScriptPathEnv         = "LLAMA_SWAP_HF_DOWNLOAD_SCRIPT"
	hfHubPathOverrideFileEnv        = "LLAMA_SWAP_HF_HUB_PATH_OVERRIDE_FILE"
	hfHubPathEnv                    = "LLAMA_SWAP_HF_HUB_PATH"
	recipesLocalDirEnv              = "LLAMA_SWAP_LOCAL_RECIPES_DIR"
	trtllmSourceImageOverrideFile   = ".llama-swap-trtllm-source-image"
	nvidiaSourceImageOverrideFile   = ".llama-swap-nvidia-source-image"
	llamacppSourceImageOverrideFile = ".llama-swap-llamacpp-source-image"
	defaultRecipesBackendSubdir     = "swap-laboratories/backend/spark-vllm-docker"
	defaultRecipesCatalogSubdir     = "recipes"
	defaultRecipesLocalSubdir       = "llama-swap/recipes"
	defaultRecipeGroupName          = "managed-recipes"
	defaultTRTLLMImageTag           = "trtllm-node"
	defaultTRTLLMSourceImage        = "nvcr.io/nvidia/tensorrt-llm/release:1.3.0rc3"
	defaultNVIDIAImageTag           = "nvcr.io/nvidia/vllm:26.01-py3"
	defaultNVIDIASourceImage        = "nvcr.io/nvidia/vllm:26.01-py3"
	defaultLLAMACPPSparkSourceImage = "llama-cpp-spark:last"
	trtllmDeploymentGuideURL        = "https://build.nvidia.com/spark/trt-llm/stacked-sparks"
	nvidiaDeploymentGuideURL        = "https://nvidia.github.io/spark-rapids-docs/"
	defaultHFDownloadScriptName     = "hf-download.sh"
	defaultHFHubRelativePath        = ".cache/huggingface/hub"
	defaultHFVLLMBackendName        = "spark-vllm-docker"
	defaultHFLLAMACPPBackendName    = "spark-llama-cpp"
	llamacppDeploymentGuideURL      = "https://github.com/ggml-org/llama.cpp/tree/master/examples/server"
	recipeMetadataKey               = "recipe_ui"
	recipeMetadataManagedField      = "managed"
	nvcrProxyAuthURL                = "https://nvcr.io/proxy_auth?scope=repository:nvidia/tensorrt-llm/release:pull"
	nvcrTagsListURL                 = "https://nvcr.io/v2/nvidia/tensorrt-llm/release/tags/list?n=2000"
	nvidiaNGCAPIURL                 = "https://catalog.ngc.nvidia.com/api/v3/orgs/nvidia/containers/vllm/versions"
	nvidiaNGCBaseURL                = "https://catalog.ngc.nvidia.com/orgs/nvidia/containers/vllm"
)

var (
	recipeRunnerRe                    = regexp.MustCompile(`(?:^|\s)(?:exec\s+)?(?:\$\{recipe_runner\}|[^\s'"]*run-recipe\.sh)\s+([^\s'"]+)`)
	recipeTpRe                        = regexp.MustCompile(`(?:^|\s)--tp\s+([0-9]+)`)
	recipeNodesRe                     = regexp.MustCompile(`(?:^|\s)-n\s+("?[^"\s]+"?|\$\{[^}]+\}|[^\s]+)`)
	recipeTemplateVarRe               = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)
	vllmExtraDockerArgsAssignRe       = regexp.MustCompile(`\bVLLM_SPARK_EXTRA_DOCKER_ARGS=(?:'[^']*'|"[^"]*"|[^\s;]+)`)
	legacyAncestorFilterSingleQuoteRe = regexp.MustCompile(`--filter 'ancestor=([^']+)'`)
	gpuMemoryUtilRe                   = regexp.MustCompile(`(?:^|\s)--gpu-memory-utilization(?:=|\s+)([0-9]*\.?[0-9]+)`)
	gpuMemoryUtilAltRe                = regexp.MustCompile(`(?:^|\s)--gpu_memory_utilization(?:=|\s+)([0-9]*\.?[0-9]+)`)
	trtllmSourceImageRe               = regexp.MustCompile(`(?m)^SOURCE_IMAGE="([^"]+)"`)
	trtllmTagVersionRe                = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:rc(\d+))?(?:\.post(\d+))?$`)
	hfHubPathOverrideMu               sync.RWMutex
	recipesBackendOverrideMu          sync.RWMutex
	recipesBackendOverride            string
	hfHubPathOverride                 string
)

type recipeCatalogMeta struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Model       string         `yaml:"model"`
	Runtime     string         `yaml:"runtime"`
	Backend     string         `yaml:"backend"`
	RecipeRef   string         `yaml:"recipe_ref"`
	SoloOnly    bool           `yaml:"solo_only"`
	ClusterOnly bool           `yaml:"cluster_only"`
	Defaults    map[string]any `yaml:"defaults"`
	Container   string         `yaml:"container"`
}

type RecipeCatalogItem struct {
	ID                    string `json:"id"`
	Ref                   string `json:"ref"`
	Path                  string `json:"path"`
	BackendDir            string `json:"backendDir,omitempty"`
	BackendKind           string `json:"backendKind,omitempty"`
	Name                  string `json:"name"`
	Description           string `json:"description"`
	Model                 string `json:"model"`
	SoloOnly              bool   `json:"soloOnly"`
	ClusterOnly           bool   `json:"clusterOnly"`
	DefaultTensorParallel int    `json:"defaultTensorParallel"`
	DefaultExtraArgs      string `json:"defaultExtraArgs,omitempty"`
	ContainerImage        string `json:"containerImage,omitempty"`
}

type RecipeManagedModel struct {
	ModelID               string   `json:"modelId"`
	RecipeRef             string   `json:"recipeRef"`
	Name                  string   `json:"name"`
	Description           string   `json:"description"`
	Aliases               []string `json:"aliases"`
	UseModelName          string   `json:"useModelName"`
	Mode                  string   `json:"mode"` // solo|cluster
	TensorParallel        int      `json:"tensorParallel"`
	Nodes                 string   `json:"nodes,omitempty"`
	ExtraArgs             string   `json:"extraArgs,omitempty"`
	ContainerImage        string   `json:"containerImage,omitempty"`
	Group                 string   `json:"group"`
	Unlisted              bool     `json:"unlisted,omitempty"`
	Managed               bool     `json:"managed"`
	BenchyTrustRemoteCode *bool    `json:"benchyTrustRemoteCode,omitempty"`
	NonPrivileged         bool     `json:"nonPrivileged,omitempty"`
	MemLimitGb            int      `json:"memLimitGb,omitempty"`
	MemSwapLimitGb        int      `json:"memSwapLimitGb,omitempty"`
	PidsLimit             int      `json:"pidsLimit,omitempty"`
	ShmSizeGb             int      `json:"shmSizeGb,omitempty"`
}

type RecipeUIState struct {
	ConfigPath  string               `json:"configPath"`
	BackendDir  string               `json:"backendDir"`
	BackendKind string               `json:"backendKind,omitempty"`
	Recipes     []RecipeCatalogItem  `json:"recipes"`
	Models      []RecipeManagedModel `json:"models"`
	Groups      []string             `json:"groups"`
}

type RecipeBackendState struct {
	BackendDir         string                      `json:"backendDir"`
	BackendSource      string                      `json:"backendSource"`
	Options            []string                    `json:"options"`
	BackendKind        string                      `json:"backendKind"`
	BackendVendor      string                      `json:"backendVendor,omitempty"`
	DeploymentGuideURL string                      `json:"deploymentGuideUrl,omitempty"`
	RepoURL            string                      `json:"repoUrl,omitempty"`
	Actions            []RecipeBackendActionInfo   `json:"actions"`
	TRTLLMImage        *RecipeBackendTRTLLMImage   `json:"trtllmImage,omitempty"`
	NVIDIAImage        *RecipeBackendNVIDIAImage   `json:"nvidiaImage,omitempty"`
	LLAMACPPImage      *RecipeBackendLLAMACPPImage `json:"llamacppImage,omitempty"`
}

type RecipeBackendActionInfo struct {
	Action      string `json:"action"`
	Label       string `json:"label"`
	CommandHint string `json:"commandHint,omitempty"`
}

type RecipeBackendTRTLLMImage struct {
	Selected        string   `json:"selected"`
	Default         string   `json:"default"`
	Latest          string   `json:"latest,omitempty"`
	UpdateAvailable bool     `json:"updateAvailable,omitempty"`
	Available       []string `json:"available,omitempty"`
	Warning         string   `json:"warning,omitempty"`
}

type RecipeBackendNVIDIAImage struct {
	Selected        string   `json:"selected"`
	Default         string   `json:"default"`
	Latest          string   `json:"latest,omitempty"`
	UpdateAvailable bool     `json:"updateAvailable,omitempty"`
	Available       []string `json:"available,omitempty"`
	Warning         string   `json:"warning,omitempty"`
}

type RecipeBackendLLAMACPPImage struct {
	Selected  string   `json:"selected"`
	Default   string   `json:"default"`
	Available []string `json:"available,omitempty"`
	Warning   string   `json:"warning,omitempty"`
}

type upsertRecipeModelRequest struct {
	ModelID               string   `json:"modelId"`
	RecipeRef             string   `json:"recipeRef"`
	Name                  string   `json:"name"`
	Description           string   `json:"description"`
	Aliases               []string `json:"aliases"`
	UseModelName          string   `json:"useModelName"`
	Mode                  string   `json:"mode"` // solo|cluster
	TensorParallel        int      `json:"tensorParallel"`
	Nodes                 string   `json:"nodes,omitempty"`
	ExtraArgs             string   `json:"extraArgs,omitempty"`
	Group                 string   `json:"group"`
	Unlisted              bool     `json:"unlisted,omitempty"`
	BenchyTrustRemoteCode *bool    `json:"benchyTrustRemoteCode,omitempty"`
	HotSwap               bool     `json:"hotSwap,omitempty"`        // If true, don't stop cluster
	ContainerImage        string   `json:"containerImage,omitempty"` // Docker container to use (overrides recipe default)
	NonPrivileged         bool     `json:"nonPrivileged,omitempty"`  // Use non-privileged mode
	MemLimitGb            int      `json:"memLimitGb,omitempty"`     // Memory limit in GB (non-privileged mode)
	MemSwapLimitGb        int      `json:"memSwapLimitGb,omitempty"` // Memory+swap limit in GB
	PidsLimit             int      `json:"pidsLimit,omitempty"`      // Process limit
	ShmSizeGb             int      `json:"shmSizeGb,omitempty"`      // Shared memory size in GB
}

func appendNonPrivilegedRecipeArgs(parts []string, req upsertRecipeModelRequest) []string {
	if !req.NonPrivileged {
		return parts
	}

	parts = append(parts, " --non-privileged")
	if req.MemLimitGb > 0 {
		parts = append(parts, fmt.Sprintf(" --mem-limit-gb %d", req.MemLimitGb))
	}
	if req.MemSwapLimitGb > 0 {
		parts = append(parts, fmt.Sprintf(" --mem-swap-limit-gb %d", req.MemSwapLimitGb))
	}
	if req.PidsLimit > 0 {
		parts = append(parts, fmt.Sprintf(" --pids-limit %d", req.PidsLimit))
	}
	if req.ShmSizeGb > 0 {
		parts = append(parts, fmt.Sprintf(" --shm-size-gb %d", req.ShmSizeGb))
	}
	return parts
}

type recipeBackendActionRequest struct {
	Action         string `json:"action"`
	SourceImage    string `json:"sourceImage,omitempty"`
	HFModel        string `json:"hfModel,omitempty"`
	HFFormat       string `json:"hfFormat,omitempty"`
	HFQuantization string `json:"hfQuantization,omitempty"`
}

type recipeBackendActionResponse struct {
	Action     string `json:"action"`
	BackendDir string `json:"backendDir"`
	Command    string `json:"command"`
	Message    string `json:"message"`
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

type recipeBackendActionStatus struct {
	Running    bool   `json:"running"`
	Action     string `json:"action,omitempty"`
	BackendDir string `json:"backendDir,omitempty"`
	Command    string `json:"command,omitempty"`
	State      string `json:"state,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

type recipeBackendHFModel struct {
	CacheDir             string `json:"cacheDir"`
	ModelID              string `json:"modelId"`
	Path                 string `json:"path"`
	SizeBytes            int64  `json:"sizeBytes"`
	ModifiedAt           string `json:"modifiedAt"`
	HasRecipe            bool   `json:"hasRecipe,omitempty"`
	ExistingRecipeRef    string `json:"existingRecipeRef,omitempty"`
	ExistingModelEntryID string `json:"existingModelEntryId,omitempty"`
}

type recipeBackendHFModelsResponse struct {
	HubPath string                 `json:"hubPath"`
	Models  []recipeBackendHFModel `json:"models"`
}

type setRecipeBackendHFHubPathRequest struct {
	HubPath string `json:"hubPath"`
}

type deleteRecipeBackendHFModelRequest struct {
	CacheDir string `json:"cacheDir"`
}

type generateRecipeBackendHFModelRequest struct {
	CacheDir string `json:"cacheDir"`
}

type recipeBackendHFRecipeResponse struct {
	CacheDir      string `json:"cacheDir"`
	ModelID       string `json:"modelId"`
	Format        string `json:"format"`
	BackendDir    string `json:"backendDir"`
	BackendKind   string `json:"backendKind"`
	RecipeRef     string `json:"recipeRef"`
	RecipePath    string `json:"recipePath"`
	ModelEntryID  string `json:"modelEntryId"`
	CreatedRecipe bool   `json:"createdRecipe"`
	Message       string `json:"message"`
}

type recipeSourceState struct {
	RecipeID  string `json:"recipeId"`
	RecipeRef string `json:"recipeRef"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type recipeSourceUpdateRequest struct {
	RecipeRef string `json:"recipeRef"`
	Content   string `json:"content"`
}

type recipeSourceCreateRequest struct {
	RecipeRef string `json:"recipeRef"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite"`
}

func (pm *ProxyManager) apiGetRecipeState(c *gin.Context) {
	state, err := pm.buildRecipeUIState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiGetRecipeBackend(c *gin.Context) {
	c.JSON(http.StatusOK, pm.recipeBackendState())
}

func (pm *ProxyManager) apiGetRecipeBackendActionStatus(c *gin.Context) {
	c.JSON(http.StatusOK, pm.recipeBackendActionStatusSnapshot())
}

func (pm *ProxyManager) apiSetRecipeBackend(c *gin.Context) {
	// Backend selection is intentionally disabled. We keep this endpoint for
	// compatibility with older UI clients and always return the current state.
	c.JSON(http.StatusOK, pm.recipeBackendState())
}

func (pm *ProxyManager) apiListRecipeBackendHFModels(c *gin.Context) {
	state, err := pm.listRecipeBackendHFModelsWithRecipeState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiSetRecipeBackendHFHubPath(c *gin.Context) {
	var req setRecipeBackendHFHubPathRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	hubPath := expandLeadingTilde(strings.TrimSpace(req.HubPath))
	if hubPath != "" {
		absPath, err := filepath.Abs(hubPath)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid hubPath: %v", err)})
			return
		}
		hubPath = absPath

		if stat, err := os.Stat(hubPath); err == nil {
			if !stat.IsDir() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "hubPath must be a directory"})
				return
			}
		} else if errors.Is(err, fs.ErrNotExist) {
			if mkErr := os.MkdirAll(hubPath, 0755); mkErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create hubPath: %v", mkErr)})
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to access hubPath: %v", err)})
			return
		}
	}

	setHFHubPathOverride(hubPath)
	if err := pm.persistHFHubPathOverride(hubPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	state, err := pm.listRecipeBackendHFModelsWithRecipeState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiDeleteRecipeBackendHFModel(c *gin.Context) {
	var req deleteRecipeBackendHFModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	cacheDir := strings.TrimSpace(req.CacheDir)
	if cacheDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cacheDir is required"})
		return
	}
	if strings.Contains(cacheDir, "/") || strings.Contains(cacheDir, "\\") || strings.Contains(cacheDir, "..") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cacheDir"})
		return
	}
	if !strings.HasPrefix(cacheDir, "models--") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cacheDir must start with models--"})
		return
	}

	hubPath := resolveHFHubPath()
	if hubPath == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hf hub path is empty"})
		return
	}

	target := filepath.Join(hubPath, cacheDir)
	hubAbs, err := filepath.Abs(hubPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("resolve hub path failed: %v", err)})
		return
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("resolve target path failed: %v", err)})
		return
	}
	if !strings.HasPrefix(targetAbs, hubAbs+string(os.PathSeparator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cacheDir path"})
		return
	}

	stat, err := os.Stat(targetAbs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("cache directory not found: %s", cacheDir)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("stat failed: %v", err)})
		return
	}
	if !stat.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cacheDir is not a directory"})
		return
	}

	if err := removeHFModelDir(c.Request.Context(), targetAbs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("delete failed: %v", err)})
		return
	}

	peerDeleteErrors := deleteHFModelFromPeerNodes(c.Request.Context(), targetAbs)
	if len(peerDeleteErrors) > 0 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": fmt.Sprintf("model deleted locally but failed on peer nodes: %s", strings.Join(peerDeleteErrors, "; ")),
		})
		return
	}

	state, err := pm.listRecipeBackendHFModelsWithRecipeState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiGenerateRecipeBackendHFModel(c *gin.Context) {
	var req generateRecipeBackendHFModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	state, err := pm.generateRecipeBackendHFModel(c.Request.Context(), req.CacheDir)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func deleteHFModelFromPeerNodes(parentCtx context.Context, modelPath string) []string {
	modelPath = strings.TrimSpace(modelPath)
	if modelPath == "" {
		return nil
	}

	discoveryCtx, cancelDiscovery := context.WithTimeout(parentCtx, 15*time.Second)
	nodes, localIP, err := discoverClusterNodeIPs(discoveryCtx)
	cancelDiscovery()
	if err != nil {
		return []string{fmt.Sprintf("cluster autodiscovery failed: %v", err)}
	}

	if len(nodes) <= 1 {
		return nil
	}

	localIP = strings.TrimSpace(localIP)
	errorsList := make([]string, 0, len(nodes))
	for _, nodeIP := range nodes {
		nodeIP = strings.TrimSpace(nodeIP)
		if nodeIP == "" {
			continue
		}
		if localIP != "" && nodeIP == localIP {
			continue
		}

		nodeCtx, cancelNode := context.WithTimeout(parentCtx, 30*time.Second)
		peerScript := fmt.Sprintf(
			"if [ -e %[1]s ]; then rm -rf %[1]s || sudo -n rm -rf %[1]s; fi",
			shellQuote(modelPath),
		)
		_, runErr := runClusterNodeShell(nodeCtx, nodeIP, false, peerScript)
		cancelNode()
		if runErr != nil {
			errorsList = append(errorsList, fmt.Sprintf("%s (%v)", nodeIP, runErr))
		}
	}
	return errorsList
}

func removeHFModelDir(ctx context.Context, targetAbs string) error {
	targetAbs = strings.TrimSpace(targetAbs)
	if targetAbs == "" {
		return fmt.Errorf("target path is empty")
	}

	if err := os.RemoveAll(targetAbs); err == nil {
		return nil
	} else {
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sudo", "-n", "rm", "-rf", "--", targetAbs)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("permission denied and sudo cleanup failed: %s", msg)
	}

	if _, statErr := os.Stat(targetAbs); statErr == nil {
		return fmt.Errorf("path still exists after sudo cleanup: %s", targetAbs)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}

	return nil
}

func (pm *ProxyManager) beginRecipeBackendAction(action, backendDir, command string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	action = strings.TrimSpace(action)
	backendDir = strings.TrimSpace(backendDir)
	command = strings.TrimSpace(command)

	pm.backendActionStatusMu.Lock()
	defer pm.backendActionStatusMu.Unlock()

	if pm.backendActionStatus.Running {
		return fmt.Errorf("action %s is already running", pm.backendActionStatus.Action)
	}

	pm.backendActionStatus = recipeBackendActionStatus{
		Running:    true,
		Action:     action,
		BackendDir: backendDir,
		Command:    command,
		State:      "running",
		StartedAt:  now,
		UpdatedAt:  now,
	}
	return nil
}

func (pm *ProxyManager) completeRecipeBackendAction(action, backendDir, command string, started time.Time, durationMs int64, outputText, errText string) {
	now := time.Now().UTC().Format(time.RFC3339)
	action = strings.TrimSpace(action)
	backendDir = strings.TrimSpace(backendDir)
	command = strings.TrimSpace(command)
	errText = strings.TrimSpace(errText)
	state := "success"
	if errText != "" {
		state = "failed"
	}

	pm.backendActionStatusMu.Lock()
	pm.backendActionStatus = recipeBackendActionStatus{
		Running:    false,
		Action:     action,
		BackendDir: backendDir,
		Command:    command,
		State:      state,
		StartedAt:  started.UTC().Format(time.RFC3339),
		UpdatedAt:  now,
		DurationMs: durationMs,
		Output:     outputText,
		Error:      errText,
	}
	pm.backendActionStatusMu.Unlock()
}

func (pm *ProxyManager) recipeBackendActionStatusSnapshot() recipeBackendActionStatus {
	pm.backendActionStatusMu.Lock()
	status := pm.backendActionStatus
	pm.backendActionStatusMu.Unlock()
	if status.UpdatedAt == "" {
		status.State = "idle"
	}
	return status
}

func (pm *ProxyManager) apiRunRecipeBackendAction(c *gin.Context) {
	var req recipeBackendActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	backendDir := strings.TrimSpace(recipesBackendDir())
	if backendDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "backend dir is empty"})
		return
	}
	if stat, err := os.Stat(backendDir); err != nil || !stat.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("backend dir not found: %s", backendDir)})
		return
	}

	backendKind := detectRecipeBackendKind(backendDir)
	hasGit := backendHasGitRepo(backendDir)
	hasBuildScript := backendHasBuildScript(backendDir)

	var cmd *exec.Cmd
	var commandText string
	var trtllmSourceImage string
	var llamacppSourceImage string

	switch action {
	case "git_pull":
		if !hasGit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "git actions are not available for this backend"})
			return
		}
		commandText = "git pull --ff-only"
		cmd = exec.CommandContext(c.Request.Context(), "git", "-C", backendDir, "pull", "--ff-only")
	case "git_pull_rebase":
		if !hasGit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "git actions are not available for this backend"})
			return
		}
		commandText = "git pull --rebase --autostash"
		cmd = exec.CommandContext(c.Request.Context(), "git", "-C", backendDir, "pull", "--rebase", "--autostash")
	case "build_vllm":
		if backendKind == "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "build_vllm is not supported for TRT-LLM backend"})
			return
		}
		if !hasBuildScript {
			script := filepath.Join(backendDir, "build-and-copy.sh")
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("build script not found: %s", script)})
			return
		}
		commandText = "./build-and-copy.sh -t vllm-node -c"
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(ctx, "bash", "./build-and-copy.sh", "-t", "vllm-node", "-c")
		cmd.Dir = backendDir
	case "build_mxfp4":
		if backendKind == "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "build_mxfp4 is not supported for TRT-LLM backend"})
			return
		}
		if !hasBuildScript {
			script := filepath.Join(backendDir, "build-and-copy.sh")
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("build script not found: %s", script)})
			return
		}
		commandText = "./build-and-copy.sh -t vllm-node-mxfp4 --exp-mxfp4 -c"
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(
			ctx,
			"bash",
			"./build-and-copy.sh",
			"-t", "vllm-node-mxfp4",
			"--exp-mxfp4",
			"-c",
		)
		cmd.Dir = backendDir
	case "build_vllm_12_0f":
		if backendKind == "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "build_vllm_12_0f is not supported for TRT-LLM backend"})
			return
		}
		if !hasBuildScript {
			script := filepath.Join(backendDir, "build-and-copy.sh")
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("build script not found: %s", script)})
			return
		}
		commandText = "./build-and-copy.sh -t vllm-node-12.0f --gpu-arch 12.0f -c"
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(
			ctx,
			"bash",
			"./build-and-copy.sh",
			"-t", "vllm-node-12.0f",
			"--gpu-arch", "12.0f",
			"-c",
		)
		cmd.Dir = backendDir
	case "build_trtllm_image":
		if backendKind != "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "build_trtllm_image is only supported for TRT-LLM backend"})
			return
		}
		if !hasBuildScript {
			script := filepath.Join(backendDir, "build-and-copy.sh")
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("build script not found: %s", script)})
			return
		}
		trtllmSourceImage = resolveTRTLLMSourceImage(backendDir, req.SourceImage)
		if trtllmSourceImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		commandText = fmt.Sprintf("./build-and-copy.sh -t %s --source-image %s -c", defaultTRTLLMImageTag, trtllmSourceImage)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(
			ctx,
			"bash",
			"./build-and-copy.sh",
			"-t", defaultTRTLLMImageTag,
			"--source-image", trtllmSourceImage,
			"-c",
		)
		cmd.Dir = backendDir
	case "pull_trtllm_image":
		if backendKind != "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pull_trtllm_image is only supported for TRT-LLM backend"})
			return
		}
		trtllmImage := resolveTRTLLMSourceImage(backendDir, req.SourceImage)
		if trtllmImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		commandText = fmt.Sprintf("docker pull %s", trtllmImage)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
		defer cancel()
		cmd = exec.CommandContext(ctx, "docker", "pull", trtllmImage)
	case "update_trtllm_image":
		if backendKind != "trtllm" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "update_trtllm_image is only supported for TRT-LLM backend"})
			return
		}
		trtllmImage := resolveTRTLLMSourceImage(backendDir, req.SourceImage)
		if trtllmImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		// If no source image specified, automatically use the latest version
		if req.SourceImage == "" {
			state := pm.recipeBackendState()
			if state.BackendKind == "trtllm" && state.TRTLLMImage != nil && state.TRTLLMImage.Latest != "" {
				trtllmImage = state.TRTLLMImage.Latest
			}
		}
		if err := persistTRTLLMSourceImage(backendDir, trtllmImage); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist trtllm image: %v", err)})
			return
		}
		commandText = fmt.Sprintf("docker pull %s && ./copy-image-to-peers.sh %s", trtllmImage, trtllmImage)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
		defer cancel()
		cmd = exec.CommandContext(ctx, "bash", "-lc", fmt.Sprintf("docker pull %s && if [ -f ./copy-image-to-peers.sh ]; then ./copy-image-to-peers.sh %s; fi", trtllmImage, trtllmImage))
	case "pull_nvidia_image":
		if backendKind != "nvidia" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pull_nvidia_image is only supported for NVIDIA backend"})
			return
		}
		nvidiaImage := resolveNVIDIASourceImage(backendDir, req.SourceImage)
		if nvidiaImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		commandText = fmt.Sprintf("docker pull %s", nvidiaImage)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
		defer cancel()
		cmd = exec.CommandContext(ctx, "docker", "pull", nvidiaImage)
	case "update_nvidia_image":
		if backendKind != "nvidia" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "update_nvidia_image is only supported for NVIDIA backend"})
			return
		}
		nvidiaImage := resolveNVIDIASourceImage(backendDir, req.SourceImage)
		if nvidiaImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		// If no source image specified, automatically use the latest version
		if req.SourceImage == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			tags, err := fetchNVIDIAReleaseTags(ctx)
			if err == nil && len(tags) > 0 {
				latestImage := latestNVIDIATag(tags)
				if latestImage != "" {
					nvidiaImage = latestImage
				}
			}
		}
		if err := persistNVIDIASourceImage(backendDir, nvidiaImage); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist nvidia image: %v", err)})
			return
		}
		commandText = fmt.Sprintf("docker pull %s", nvidiaImage)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Minute)
		defer cancel()
		cmd = exec.CommandContext(ctx, "docker", "pull", nvidiaImage)
	case "build_llamacpp":
		llamacppSourceImage = resolveLLAMACPPSourceImage(backendDir, req.SourceImage)
		if llamacppSourceImage == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "source image is empty"})
			return
		}
		if err := persistLLAMACPPSourceImage(backendDir, llamacppSourceImage); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to persist llamacpp image: %v", err)})
			return
		}
		autodiscoverPath := clusterAutodiscoverPath()
		commandText = fmt.Sprintf("build llama.cpp latest release and sync image %s to autodiscovered nodes (%s)", llamacppSourceImage, autodiscoverPath)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(ctx, "bash", "-lc", buildLLAMACPPBuildAndSyncScript(backendDir, autodiscoverPath, llamacppSourceImage))
	case "download_hf_model":
		hfModel := strings.TrimSpace(req.HFModel)
		if hfModel == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hfModel is required (format: org/model)"})
			return
		}
		if strings.ContainsAny(hfModel, " \t\r\n") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hfModel must not contain whitespace"})
			return
		}
		hfFormat := normalizeHFFormat(req.HFFormat)
		if hfFormat == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hfFormat must be gguf or safetensors"})
			return
		}
		hfQuantization := strings.TrimSpace(req.HFQuantization)
		if strings.ContainsAny(hfQuantization, " \t\r\n") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hfQuantization must not contain whitespace"})
			return
		}

		scriptPath := resolveHFDownloadScriptPath()
		if !backendHasHFDownloadScript() {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("hf download script not found or not executable: %s", scriptPath)})
			return
		}
		args := []string{hfModel, "--format", hfFormat}
		if hfQuantization != "" {
			args = append(args, "--quantization", hfQuantization)
		}
		args = append(args, "-c", "--copy-parallel")

		commandText = commandWithArgs(scriptPath, args...)
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		cmd = exec.CommandContext(ctx, scriptPath, args...)
		hubPath := resolveHFHubPath()
		if hubPath != "" {
			cmd.Env = append(os.Environ(), "HF_HUB_PATH="+hubPath, hfHubPathEnv+"="+hubPath)
		}
		cmd.Dir = filepath.Dir(scriptPath)
		ensureCommandPathIncludesUserLocalBin(cmd)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported action"})
		return
	}

	if beginErr := pm.beginRecipeBackendAction(action, backendDir, commandText); beginErr != nil {
		c.JSON(http.StatusConflict, gin.H{"error": beginErr.Error()})
		return
	}

	started := time.Now()
	output, err := cmd.CombinedOutput()
	durationMs := time.Since(started).Milliseconds()
	outputText := strings.TrimSpace(string(output))
	outputText = tailString(outputText, 120000)

	if err != nil {
		pm.completeRecipeBackendAction(action, backendDir, commandText, started, durationMs, outputText, err.Error())
		pm.proxyLogger.Errorf("backend action failed action=%s dir=%s err=%v", action, backendDir, err)
		errMsg := fmt.Sprintf("action %s failed: %v", action, err)
		if action == "git_pull" {
			lowerOut := strings.ToLower(outputText)
			if strings.Contains(lowerOut, "diverging branches") || strings.Contains(lowerOut, "can't be fast-forwarded") {
				errMsg = "action git_pull failed: backend has diverging history. Use 'Git Pull Rebase' to reconcile local commits with upstream."
			}
		}
		c.JSON(http.StatusBadGateway, gin.H{
			"error":      errMsg,
			"action":     action,
			"backendDir": backendDir,
			"command":    commandText,
			"output":     outputText,
			"durationMs": durationMs,
		})
		return
	}

	if action == "build_trtllm_image" || action == "update_trtllm_image" {
		_ = persistTRTLLMSourceImage(backendDir, trtllmSourceImage)
	}
	if action == "build_llamacpp" {
		_ = persistLLAMACPPSourceImage(backendDir, llamacppSourceImage)
	}

	pm.completeRecipeBackendAction(action, backendDir, commandText, started, durationMs, outputText, "")
	pm.proxyLogger.Infof("backend action completed action=%s dir=%s durationMs=%d", action, backendDir, durationMs)
	c.JSON(http.StatusOK, recipeBackendActionResponse{
		Action:     action,
		BackendDir: backendDir,
		Command:    commandText,
		Message:    fmt.Sprintf("Action %s completed successfully.", action),
		Output:     outputText,
		DurationMs: durationMs,
	})
}

func (pm *ProxyManager) apiGetRecipeSource(c *gin.Context) {
	recipeRef := strings.TrimSpace(c.Query("recipeRef"))
	state, err := pm.readRecipeSourceState(recipeRef)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiSaveRecipeSource(c *gin.Context) {
	var req recipeSourceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	recipeRef := strings.TrimSpace(req.RecipeRef)
	if recipeRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recipeRef is required"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recipe content is required"})
		return
	}

	if err := validateRecipeYAML(req.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid recipe YAML: %v", err)})
		return
	}

	state, err := pm.saveRecipeSourceState(recipeRef, req.Content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pm.syncRecipesToClusterAsync("save_recipe")
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiCreateRecipeSource(c *gin.Context) {
	var req recipeSourceCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	recipeRef := strings.TrimSpace(req.RecipeRef)
	if recipeRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recipeRef is required"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recipe content is required"})
		return
	}

	if err := validateRecipeYAML(req.Content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid recipe YAML: %v", err)})
		return
	}

	state, err := pm.createRecipeSourceState(recipeRef, req.Content, req.Overwrite)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pm.syncRecipesToClusterAsync("create_recipe")
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) syncRecipesToClusterAsync(reason string) {
	configPath, err := pm.getConfigPath()
	if err != nil {
		pm.proxyLogger.Warnf("recipe sync skipped: %v", err)
		return
	}

	root := filepath.Dir(configPath)
	script := filepath.Join(root, "scripts", "sync-recipes.sh")
	if !isExecutableFile(script) {
		pm.proxyLogger.Warnf("recipe sync skipped: script not executable: %s", script)
		return
	}

	label := strings.TrimSpace(reason)
	if label != "" {
		label = " (" + label + ")"
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(ctx, script)
		cmd.Dir = root
		output, runErr := cmd.CombinedOutput()
		outText := strings.TrimSpace(string(output))
		if runErr != nil {
			if outText == "" {
				outText = runErr.Error()
			}
			pm.proxyLogger.Errorf("recipe sync failed%s: %s", label, outText)
			return
		}
		if outText != "" {
			pm.proxyLogger.Infof("recipe sync done%s: %s", label, outText)
		} else {
			pm.proxyLogger.Infof("recipe sync done%s", label)
		}
	}()
}

func validateRecipeYAML(content string) error {
	var parsed any
	return yaml.Unmarshal([]byte(content), &parsed)
}

func (pm *ProxyManager) apiUpsertRecipeModel(c *gin.Context) {
	var req upsertRecipeModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	state, err := pm.upsertRecipeModel(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) apiDeleteRecipeModel(c *gin.Context) {
	modelID := strings.TrimSpace(c.Param("id"))
	if modelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model id is required"})
		return
	}

	state, err := pm.deleteRecipeModel(modelID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) buildRecipeUIState() (RecipeUIState, error) {
	configPath, err := pm.getConfigPath()
	if err != nil {
		return RecipeUIState{}, err
	}

	catalog, catalogByID, err := loadRecipeCatalog("")
	if err != nil {
		return RecipeUIState{}, err
	}

	root, err := loadConfigRawMap(configPath)
	if err != nil {
		return RecipeUIState{}, err
	}

	modelsMap := getMap(root, "models")
	groupsMap := getMap(root, "groups")

	models := make([]RecipeManagedModel, 0, len(modelsMap))
	for modelID, raw := range modelsMap {
		modelMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rm, ok := toRecipeManagedModel(modelID, modelMap, groupsMap)
		if ok && recipeManagedModelInCatalog(rm, catalogByID) {
			models = append(models, rm)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ModelID < models[j].ModelID })

	groupNames := sortedGroupNames(groupsMap)
	return RecipeUIState{
		ConfigPath:  configPath,
		BackendDir:  recipesCatalogPrimaryDir(),
		BackendKind: "mixed",
		Recipes:     catalog,
		Models:      models,
		Groups:      groupNames,
	}, nil
}

func (pm *ProxyManager) recipeBackendState() RecipeBackendState {
	current, source := recipesBackendDirWithSource()
	options := []string{}
	if strings.TrimSpace(current) != "" {
		options = append(options, current)
	}

	kind := detectRecipeBackendKind(current)
	repoURL := backendGitRemoteOrigin(current)
	actions := recipeBackendActionsForKind(kind, current, repoURL)

	state := RecipeBackendState{
		BackendDir:    current,
		BackendSource: source,
		Options:       options,
		BackendKind:   kind,
		BackendVendor: recipeBackendVendor(kind),
		RepoURL:       repoURL,
		Actions:       actions,
	}
	if kind == "trtllm" {
		state.TRTLLMImage = buildTRTLLMImageState(current)
		state.DeploymentGuideURL = trtllmDeploymentGuideURL
	}
	if kind == "nvidia" {
		state.NVIDIAImage = buildNVIDIAImageState(current)
		state.DeploymentGuideURL = nvidiaDeploymentGuideURL
	}
	if kind == "llamacpp" {
		state.LLAMACPPImage = buildLLAMACPPImageState(current)
		state.DeploymentGuideURL = llamacppDeploymentGuideURL
	}
	return state
}

func (pm *ProxyManager) upsertRecipeModel(parentCtx context.Context, req upsertRecipeModelRequest) (RecipeUIState, error) {
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		return RecipeUIState{}, errors.New("modelId is required")
	}
	recipeRefInput := strings.TrimSpace(req.RecipeRef)
	if recipeRefInput == "" {
		return RecipeUIState{}, errors.New("recipeRef is required")
	}

	configPath, err := pm.getConfigPath()
	if err != nil {
		return RecipeUIState{}, err
	}

	catalog, catalogByID, err := loadRecipeCatalog("")
	if err != nil {
		return RecipeUIState{}, err
	}
	_ = catalog

	resolvedRecipeRef, catalogRecipe, err := resolveRecipeRef(recipeRefInput, catalogByID)
	if err != nil {
		return RecipeUIState{}, err
	}
	recipeBackendDir := strings.TrimSpace(catalogRecipe.BackendDir)
	if recipeBackendDir == "" {
		return RecipeUIState{}, fmt.Errorf("recipe %s backend not resolved; set backend: in recipe yaml", resolvedRecipeRef)
	}
	backendKind := detectRecipeBackendKind(recipeBackendDir)
	recipePath := strings.TrimSpace(catalogRecipe.Path)

	requestedNodes := strings.TrimSpace(req.Nodes)
	requestedNodeList := splitAndNormalizeNodes(requestedNodes)

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		if catalogRecipe.SoloOnly {
			mode = "solo"
		} else {
			mode = "cluster"
		}
	}

	tp := req.TensorParallel
	if tp <= 0 {
		tp = catalogRecipe.DefaultTensorParallel
	}
	if tp <= 0 {
		tp = 1
	}

	root, err := loadConfigRawMap(configPath)
	if err != nil {
		return RecipeUIState{}, err
	}
	ensureRecipeMacros(root, configPath)
	modelsMap := getMap(root, "models")
	groupsMap := getMap(root, "groups")

	existing := getMap(modelsMap, modelID)
	resolvedExtraArgs := strings.TrimSpace(req.ExtraArgs)
	if resolvedExtraArgs == "" {
		existingMeta := getMap(existing, "metadata")
		existingRecipeMeta := getMap(existingMeta, recipeMetadataKey)
		resolvedExtraArgs = strings.TrimSpace(getString(existingRecipeMeta, "extra_args"))
	}
	if resolvedExtraArgs == "" {
		resolvedExtraArgs = strings.TrimSpace(catalogRecipe.DefaultExtraArgs)
	}

	autoNodeSelection := ""
	if requestedNodes == "" && mode == "cluster" && tp <= 1 && !catalogRecipe.ClusterOnly {
		gpuUtil := resolveGPUMemoryUtilization(recipePath, resolvedExtraArgs)
		selected, selectErr := selectBestFitNode(parentCtx, gpuUtil)
		if selectErr != nil {
			return RecipeUIState{}, selectErr
		}
		autoNodeSelection = selected
	}

	singleNodeSelection := ""
	if len(requestedNodeList) == 1 && tp <= 1 {
		singleNodeSelection = requestedNodeList[0]
	}
	if singleNodeSelection == "" && autoNodeSelection != "" {
		singleNodeSelection = autoNodeSelection
	}

	// For TP=1 with an explicit node, run that model on the selected node directly.
	if singleNodeSelection != "" {
		mode = "solo"
	} else if requestedNodes != "" && tp <= 1 {
		mode = "cluster"
	}

	if mode != "solo" && mode != "cluster" {
		return RecipeUIState{}, errors.New("mode must be 'solo' or 'cluster'")
	}
	if catalogRecipe.SoloOnly && mode != "solo" {
		return RecipeUIState{}, fmt.Errorf("recipe %s requires solo mode", recipeRefInput)
	}
	if catalogRecipe.ClusterOnly && mode != "cluster" {
		return RecipeUIState{}, fmt.Errorf("recipe %s requires cluster mode", recipeRefInput)
	}

	nodes := requestedNodes
	if singleNodeSelection != "" {
		nodes = singleNodeSelection
	}
	if mode == "cluster" && nodes == "" {
		if expr, ok := backendMacroExprForKind(root, backendKind, "nodes"); ok {
			nodes = expr
		} else {
			return RecipeUIState{}, errors.New("nodes is required for cluster mode (backend nodes macro not found)")
		}
	}

	groupName := strings.TrimSpace(req.Group)
	if groupName == "" {
		groupName = defaultRecipeGroupName
	}
	// Keep single-node pinned workloads isolated so two models can load/run in
	// parallel on different nodes without contending in the same swap group.
	if singleNodeSelection != "" {
		if groupName == defaultRecipeGroupName || strings.HasPrefix(groupName, defaultRecipeGroupName+"-") {
			groupName = fmt.Sprintf("%s-%s", defaultRecipeGroupName, sanitizeGroupSuffix(singleNodeSelection))
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = modelID
	}
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = catalogRecipe.Description
	}

	useModelName := strings.TrimSpace(req.UseModelName)
	if useModelName == "" {
		useModelName = catalogRecipe.Model
	}

	modelEntry := cloneMap(existing)
	modelEntry["name"] = name
	modelEntry["description"] = description
	proxyTarget := "http://127.0.0.1:${PORT}"
	if singleNodeSelection != "" {
		proxyTarget = fmt.Sprintf("http://%s:${PORT}", singleNodeSelection)
	}
	modelEntry["proxy"] = proxyTarget
	modelEntry["checkEndpoint"] = "/health"
	modelEntry["ttl"] = 0
	modelEntry["useModelName"] = useModelName
	modelEntry["unlisted"] = req.Unlisted
	modelEntry["aliases"] = cleanAliases(req.Aliases)

	// Hot swap mode: don't stop cluster, just swap model. llama.cpp backend
	// currently runs in solo mode, so hot swap is effectively disabled there.
	hotSwap := req.HotSwap && mode == "cluster" && backendKind != "llamacpp"
	runtimeCachePolicyEnabled := backendKind == "vllm"
	runtimeCachePolicyInCommand := runtimeCachePolicyEnabled && !hotSwap
	runtimeCachePolicyAssignment := vllmRuntimeCacheAssignment(
		mergeVLLMRuntimeCacheExtraDockerArgs("", "${user_home}"),
		false,
	)

	// If custom container specified, update the recipe file
	customContainer := strings.TrimSpace(req.ContainerImage)
	var containerImageToStore string
	if customContainer != "" {
		if recipePath != "" {
			if recipeData, err := os.ReadFile(recipePath); err == nil {
				var newLines []string
				containerFound := false
				for _, line := range strings.Split(string(recipeData), "\n") {
					if strings.HasPrefix(line, "container:") {
						newLines = append(newLines, fmt.Sprintf("container: %s", customContainer))
						containerImageToStore = customContainer
						containerFound = true
					} else {
						newLines = append(newLines, line)
					}
				}
				if containerFound {
					if err := os.WriteFile(recipePath, []byte(strings.Join(newLines, "\n")), 0644); err != nil {
						return RecipeUIState{}, fmt.Errorf("failed to update recipe container in %s: %w", recipePath, err)
					}
				}
			}
		}
	} else {
		// Read container from recipe file for metadata
		if recipePath != "" {
			if recipeData, err := os.ReadFile(recipePath); err == nil {
				for _, line := range strings.Split(string(recipeData), "\n") {
					if strings.HasPrefix(line, "container:") {
						containerImageToStore = strings.TrimSpace(strings.TrimPrefix(line, "container:"))
						break
					}
				}
			}
		}
	}
	if strings.TrimSpace(containerImageToStore) == "" {
		containerImageToStore = strings.TrimSpace(catalogRecipe.ContainerImage)
	}

	// In hot-swap mode we keep container/runtime alive, but force-stop any
	// active serve process before launching a new model to avoid stale state.
	hotSwapStopExpr := backendStopExprWithContainer(backendKind, containerImageToStore)

	// For vLLM cluster launches, perform a conditional reset when an existing
	// local vLLM container is unhealthy. This avoids unnecessary teardown/restart
	// as if it were a healthy multi-node Ray cluster.
	clusterResetPrefix := ""
	if !hotSwap && mode == "cluster" && backendKind == "vllm" {
		clusterResetPrefix = buildVLLMClusterResetExpr(recipeBackendDir, containerImageToStore)
	}

	// cmdStop is used by "Unload" / "Unload All" and must stop only the
	// model runtime without tearing down cluster nodes.
	cmdStopExpr := hotSwapStopExpr
	stopPrefix := ""
	if hotSwap {
		stopPrefix = hotSwapStopExpr + "; "
	}
	if singleNodeSelection != "" {
		remoteStopInner := "bash -lc " + strconv.Quote(cmdStopExpr)
		cmdStopExpr = fmt.Sprintf(
			"exec ssh -o BatchMode=yes -o StrictHostKeyChecking=no %s %s",
			quoteForCommand(singleNodeSelection),
			strconv.Quote(remoteStopInner),
		)
	}

	runner := ""
	if hasMacro(root, "recipe_runner") {
		runner = "${recipe_runner}"
	}
	if runner == "" {
		configRunner := filepath.Join(filepath.Dir(configPath), "run-recipe.sh")
		if isExecutableFile(configRunner) {
			runner = configRunner
		}
	}
	if runner == "" {
		backendRunner := filepath.Join(recipeBackendDir, "run-recipe.sh")
		if isExecutableFile(backendRunner) {
			runner = backendRunner
		}
	}
	if strings.TrimSpace(runner) == "" {
		return RecipeUIState{}, fmt.Errorf("recipe runner not found for %s", resolvedRecipeRef)
	}

	var cmd string
	if singleNodeSelection != "" {
		ensureRemoteBackend := buildRemoteBackendEnsureExpr(singleNodeSelection, recipeBackendDir)

		var remoteCmdParts []string
		if runtimeCachePolicyInCommand {
			remoteCmdParts = append(remoteCmdParts, runtimeCachePolicyAssignment, " ")
		}
		remoteCmdParts = append(remoteCmdParts, "exec ", runner, " ", quoteForCommand(resolvedRecipeRef), " --solo")
		if tp > 0 {
			remoteCmdParts = append(remoteCmdParts, " --tp ", strconv.Itoa(tp))
		}
		remoteCmdParts = append(remoteCmdParts, " --port ${PORT}")
		remoteCmdParts = appendNonPrivilegedRecipeArgs(remoteCmdParts, req)
		if resolvedExtraArgs != "" {
			remoteCmdParts = append(remoteCmdParts, " ", quoteForCommand(resolvedExtraArgs))
		}
		remoteInner := strings.Join(remoteCmdParts, "")
		remoteOuter := "bash -lc " + strconv.Quote(remoteInner)
		cmd = fmt.Sprintf(
			"bash -lc '%sexec ssh -o BatchMode=yes -o StrictHostKeyChecking=no %s %s'",
			ensureRemoteBackend,
			quoteForCommand(singleNodeSelection),
			strconv.Quote(remoteOuter),
		)
	} else if hotSwap {
		renderedVLLMCmd, err := buildHotSwapVLLMCommand(catalogRecipe.Path, tp, resolvedExtraArgs)
		if err != nil {
			return RecipeUIState{}, err
		}
		cmd = fmt.Sprintf(
			"bash -lc '%s; if [ -z \"${VLLM_CONTAINER:-}\" ]; then VLLM_CONTAINER=\"$(%s)\"; fi; if [ -z \"$VLLM_CONTAINER\" ]; then echo \"No running vLLM container found\" >&2; exit 1; fi; exec docker exec -i \"$VLLM_CONTAINER\" bash -i -c %s'",
			hotSwapStopExpr,
			buildVLLMContainerDetectExpr(containerImageToStore),
			strconv.Quote(renderedVLLMCmd),
		)
	} else {
		var cmdParts []string
		cmdParts = append(cmdParts, "bash -lc '", clusterResetPrefix, stopPrefix)
		if runtimeCachePolicyInCommand {
			cmdParts = append(cmdParts, runtimeCachePolicyAssignment, " ")
		}
		cmdParts = append(cmdParts, "exec ", runner, " ", quoteForCommand(resolvedRecipeRef))
		if mode == "solo" {
			cmdParts = append(cmdParts, " --solo")
		} else {
			cmdParts = append(cmdParts, " -n ", quoteForCommand(nodes))
		}
		if tp > 0 {
			cmdParts = append(cmdParts, " --tp ", strconv.Itoa(tp))
		}
		cmdParts = append(cmdParts, " --port ${PORT}")
		cmdParts = appendNonPrivilegedRecipeArgs(cmdParts, req)
		if resolvedExtraArgs != "" {
			cmdParts = append(cmdParts, " ", quoteForCommand(resolvedExtraArgs))
		}
		cmdParts = append(cmdParts, "'")
		cmd = strings.Join(cmdParts, "")
	}

	modelEntry["cmd"] = cmd
	modelEntry["cmdStop"] = fmt.Sprintf("bash -lc '%s'", cmdStopExpr)

	meta := getMap(existing, "metadata")
	if len(meta) == 0 {
		meta = map[string]any{}
	}
	recipeMeta := map[string]any{
		recipeMetadataManagedField: true,
		"recipe_ref":               resolvedRecipeRef,
		"mode":                     mode,
		"tensor_parallel":          tp,
		"nodes":                    nodes,
		"extra_args":               resolvedExtraArgs,
		"group":                    groupName,
		"backend_dir":              recipeBackendDir,
		"hot_swap":                 hotSwap,
		"container_image":          containerImageToStore,
		"non_privileged":           req.NonPrivileged,
		"mem_limit_gb":             req.MemLimitGb,
		"mem_swap_limit_gb":        req.MemSwapLimitGb,
		"pids_limit":               req.PidsLimit,
		"shm_size_gb":              req.ShmSizeGb,
	}
	if runtimeCachePolicyEnabled {
		recipeMeta["runtime_cache_policy_enabled"] = true
		recipeMeta["runtime_cache_policy_version"] = 1
	}
	meta[recipeMetadataKey] = recipeMeta
	if req.BenchyTrustRemoteCode != nil {
		benchyMeta := getMap(meta, "benchy")
		benchyMeta["trust_remote_code"] = *req.BenchyTrustRemoteCode
		meta["benchy"] = benchyMeta
	}
	modelEntry["metadata"] = meta

	modelsMap[modelID] = modelEntry
	root["models"] = modelsMap

	removeModelFromAllGroups(groupsMap, modelID)
	group := getMap(groupsMap, groupName)
	if _, ok := group["swap"]; !ok {
		group["swap"] = true
	}
	// Managed recipe groups should not be globally exclusive: models pinned to
	// different nodes must be able to run at the same time.
	if groupName == defaultRecipeGroupName || strings.HasPrefix(groupName, defaultRecipeGroupName+"-") {
		group["exclusive"] = false
	} else if _, ok := group["exclusive"]; !ok {
		group["exclusive"] = false
	}
	members := append(groupMembers(group), modelID)
	group["members"] = uniqueStrings(members)
	groupsMap[groupName] = group
	root["groups"] = groupsMap

	if err := writeConfigRawMap(configPath, root); err != nil {
		return RecipeUIState{}, err
	}

	if conf, err := config.LoadConfig(configPath); err == nil {
		pm.applyConfigAndSyncProcessGroups(normalizeLegacyVLLMConfigCommands(conf))
	}
	return pm.buildRecipeUIState()
}

func (pm *ProxyManager) deleteRecipeModel(modelID string) (RecipeUIState, error) {
	configPath, err := pm.getConfigPath()
	if err != nil {
		return RecipeUIState{}, err
	}

	root, err := loadConfigRawMap(configPath)
	if err != nil {
		return RecipeUIState{}, err
	}
	ensureRecipeMacros(root, configPath)

	modelsMap := getMap(root, "models")
	if _, ok := modelsMap[modelID]; !ok {
		return RecipeUIState{}, fmt.Errorf("model %s not found", modelID)
	}
	delete(modelsMap, modelID)
	root["models"] = modelsMap

	groupsMap := getMap(root, "groups")
	removeModelFromAllGroups(groupsMap, modelID)
	root["groups"] = groupsMap

	if err := writeConfigRawMap(configPath, root); err != nil {
		return RecipeUIState{}, err
	}

	if conf, err := config.LoadConfig(configPath); err == nil {
		pm.applyConfigAndSyncProcessGroups(normalizeLegacyVLLMConfigCommands(conf))
	}
	return pm.buildRecipeUIState()
}

func recipesBackendDir() string {
	dir, _ := recipesBackendDirWithSource()
	return dir
}

func recipesBackendDirWithSource() (string, string) {
	if v := strings.TrimSpace(getRecipesBackendOverride()); v != "" {
		return v, "override"
	}
	if v := strings.TrimSpace(os.Getenv(recipesBackendDirEnv)); v != "" {
		return v, "env"
	}
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, defaultRecipesBackendSubdir), "default"
	}
	return defaultRecipesBackendSubdir, "default"
}

func userHomeDir() string {
	if v := strings.TrimSpace(os.Getenv("HOME")); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return strings.TrimSpace(home)
	}
	return ""
}

func setRecipesBackendOverride(path string) {
	recipesBackendOverrideMu.Lock()
	recipesBackendOverride = strings.TrimSpace(path)
	recipesBackendOverrideMu.Unlock()
}

func getRecipesBackendOverride() string {
	recipesBackendOverrideMu.RLock()
	defer recipesBackendOverrideMu.RUnlock()
	return recipesBackendOverride
}

func setHFHubPathOverride(path string) {
	hfHubPathOverrideMu.Lock()
	hfHubPathOverride = strings.TrimSpace(path)
	hfHubPathOverrideMu.Unlock()
}

func getHFHubPathOverride() string {
	hfHubPathOverrideMu.RLock()
	defer hfHubPathOverrideMu.RUnlock()
	return hfHubPathOverride
}

func isRecipeBackendDir(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if stat, err := os.Stat(path); err != nil || !stat.IsDir() {
		return false
	}
	if stat, err := os.Stat(filepath.Join(path, "run-recipe.sh")); err != nil || stat.IsDir() {
		return false
	}
	if stat, err := os.Stat(filepath.Join(path, "recipes")); err != nil || !stat.IsDir() {
		return false
	}
	return true
}

func discoverRecipeBackendsFromRoot(root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	root = expandLeadingTilde(root)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if stat, err := os.Stat(root); err != nil || !stat.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if isRecipeBackendDir(candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

func detectRecipeBackendKind(backendDir string) string {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(backendDir)))
	switch {
	case strings.Contains(base, "trtllm"):
		return "trtllm"
	case strings.Contains(base, "sqlang"):
		return "sqlang"
	case strings.Contains(base, "llama-cpp") || strings.Contains(base, "llamacpp"):
		return "llamacpp"
	case strings.Contains(base, "vllm"):
		return "vllm"
	default:
		return "custom"
	}
}

func backendHasGitRepo(backendDir string) bool {
	if backendDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(backendDir, ".git")); err == nil {
		return true
	}
	return false
}

func backendHasBuildScript(backendDir string) bool {
	if backendDir == "" {
		return false
	}
	if stat, err := os.Stat(filepath.Join(backendDir, "build-and-copy.sh")); err == nil {
		return !stat.IsDir()
	}
	return false
}

func resolveHFDownloadScriptPath() string {
	toAbs := func(path string) string {
		if path == "" {
			return ""
		}
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}

	if v := strings.TrimSpace(os.Getenv(hfDownloadScriptPathEnv)); v != "" {
		return toAbs(expandLeadingTilde(v))
	}

	candidate := filepath.FromSlash(defaultHFDownloadScriptName)
	if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
		return toAbs(candidate)
	}

	if wd, err := os.Getwd(); err == nil {
		fromWD := filepath.Join(wd, filepath.FromSlash(defaultHFDownloadScriptName))
		if stat, err := os.Stat(fromWD); err == nil && !stat.IsDir() {
			return toAbs(fromWD)
		}
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(exeDir, filepath.FromSlash(defaultHFDownloadScriptName)),
			filepath.Join(exeDir, "..", filepath.FromSlash(defaultHFDownloadScriptName)),
			filepath.Join(exeDir, "..", "..", filepath.FromSlash(defaultHFDownloadScriptName)),
		} {
			if stat, err := os.Stat(c); err == nil && !stat.IsDir() {
				return toAbs(c)
			}
		}
	}
	if home := userHomeDir(); home != "" {
		fromRepo := filepath.Join(home, "swap-laboratories", defaultHFDownloadScriptName)
		if stat, err := os.Stat(fromRepo); err == nil && !stat.IsDir() {
			return toAbs(fromRepo)
		}
	}

	return toAbs(candidate)
}

func backendHasHFDownloadScript() bool {
	scriptPath := strings.TrimSpace(resolveHFDownloadScriptPath())
	if scriptPath == "" {
		return false
	}
	if stat, err := os.Stat(scriptPath); err == nil {
		return !stat.IsDir() && (stat.Mode().Perm()&0111) != 0
	}
	return false
}

func resolveHFHubPath() string {
	if v := strings.TrimSpace(getHFHubPathOverride()); v != "" {
		return expandLeadingTilde(v)
	}
	if v := strings.TrimSpace(os.Getenv(hfHubPathEnv)); v != "" {
		return expandLeadingTilde(v)
	}
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, filepath.FromSlash(defaultHFHubRelativePath))
	}
	return ""
}

func decodeHFModelIDFromCacheDir(cacheDir string) string {
	name := strings.TrimPrefix(strings.TrimSpace(cacheDir), "models--")
	if name == "" {
		return ""
	}
	parts := strings.Split(name, "--")
	if len(parts) < 2 {
		return name
	}
	return parts[0] + "/" + strings.Join(parts[1:], "--")
}

func dirSizeBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(entryPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

type recipeBackendHFRecipeMatch struct {
	RecipeRef    string
	ModelEntryID string
}

func normalizeHFModelKey(input string) string {
	return strings.ToLower(strings.TrimSpace(input))
}

func setHFRecipeModelMatch(matches map[string]recipeBackendHFRecipeMatch, key string, match recipeBackendHFRecipeMatch) {
	normalized := normalizeHFModelKey(key)
	if normalized == "" {
		return
	}
	if _, exists := matches[normalized]; exists {
		return
	}
	matches[normalized] = match
}

func (pm *ProxyManager) hfRecipeModelMatches() map[string]recipeBackendHFRecipeMatch {
	matches := map[string]recipeBackendHFRecipeMatch{}

	configPath, err := pm.getConfigPath()
	if err != nil {
		return matches
	}

	root, err := loadConfigRawMap(configPath)
	if err != nil {
		return matches
	}

	modelsMap := getMap(root, "models")
	for modelEntryID, raw := range modelsMap {
		modelMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		meta := getMap(modelMap, "metadata")
		recipeMeta := getMap(meta, recipeMetadataKey)
		recipeRef := strings.TrimSpace(getString(recipeMeta, "recipe_ref"))
		if recipeRef == "" {
			continue
		}

		match := recipeBackendHFRecipeMatch{
			RecipeRef:    recipeRef,
			ModelEntryID: strings.TrimSpace(modelEntryID),
		}
		setHFRecipeModelMatch(matches, getString(modelMap, "useModelName"), match)
		setHFRecipeModelMatch(matches, modelEntryID, match)
	}

	return matches
}

func (pm *ProxyManager) listRecipeBackendHFModelsWithRecipeState() (recipeBackendHFModelsResponse, error) {
	state, err := listRecipeBackendHFModels()
	if err != nil {
		return recipeBackendHFModelsResponse{}, err
	}

	matches := pm.hfRecipeModelMatches()
	if len(matches) == 0 || len(state.Models) == 0 {
		return state, nil
	}

	for idx := range state.Models {
		model := &state.Models[idx]
		match, ok := matches[normalizeHFModelKey(model.ModelID)]
		if !ok {
			match, ok = matches[normalizeHFModelKey(model.CacheDir)]
		}
		if !ok {
			continue
		}
		model.HasRecipe = true
		model.ExistingRecipeRef = match.RecipeRef
		model.ExistingModelEntryID = match.ModelEntryID
	}

	return state, nil
}

func listRecipeBackendHFModels() (recipeBackendHFModelsResponse, error) {
	hubPath := resolveHFHubPath()
	if hubPath == "" {
		return recipeBackendHFModelsResponse{HubPath: "", Models: []recipeBackendHFModel{}}, nil
	}

	entries, err := os.ReadDir(hubPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return recipeBackendHFModelsResponse{HubPath: hubPath, Models: []recipeBackendHFModel{}}, nil
		}
		return recipeBackendHFModelsResponse{}, fmt.Errorf("read hf hub path failed: %w", err)
	}

	models := make([]recipeBackendHFModel, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cacheDir := strings.TrimSpace(entry.Name())
		if cacheDir == "" || !strings.HasPrefix(cacheDir, "models--") {
			continue
		}
		modelPath := filepath.Join(hubPath, cacheDir)
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		models = append(models, recipeBackendHFModel{
			CacheDir:   cacheDir,
			ModelID:    decodeHFModelIDFromCacheDir(cacheDir),
			Path:       modelPath,
			SizeBytes:  dirSizeBytes(modelPath),
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].ModifiedAt > models[j].ModifiedAt
	})

	return recipeBackendHFModelsResponse{HubPath: hubPath, Models: models}, nil
}

type autoGeneratedHFRecipe struct {
	RecipeVersion string         `yaml:"recipe_version"`
	RecipeRef     string         `yaml:"recipe_ref,omitempty"`
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description,omitempty"`
	Model         string         `yaml:"model"`
	Runtime       string         `yaml:"runtime,omitempty"`
	Backend       string         `yaml:"backend,omitempty"`
	Container     string         `yaml:"container"`
	Defaults      map[string]any `yaml:"defaults"`
	GGUFFile      string         `yaml:"gguf_file,omitempty"`
	Command       string         `yaml:"command"`
}

func validateHFCacheDir(cacheDir string) (string, error) {
	cacheDir = strings.TrimSpace(cacheDir)
	if cacheDir == "" {
		return "", errors.New("cacheDir is required")
	}
	if strings.Contains(cacheDir, "/") || strings.Contains(cacheDir, "\\") || strings.Contains(cacheDir, "..") {
		return "", errors.New("invalid cacheDir")
	}
	if !strings.HasPrefix(cacheDir, "models--") {
		return "", errors.New("cacheDir must start with models--")
	}
	return cacheDir, nil
}

func resolveHFModelDirFromCacheDir(cacheDir string) (string, string, string, error) {
	cacheDir, err := validateHFCacheDir(cacheDir)
	if err != nil {
		return "", "", "", err
	}

	hubPath := resolveHFHubPath()
	if hubPath == "" {
		return "", "", "", errors.New("hf hub path is empty")
	}

	target := filepath.Join(hubPath, cacheDir)
	hubAbs, err := filepath.Abs(hubPath)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve hub path failed: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve target path failed: %w", err)
	}
	if targetAbs != hubAbs && !strings.HasPrefix(targetAbs, hubAbs+string(os.PathSeparator)) {
		return "", "", "", errors.New("invalid cacheDir path")
	}

	stat, err := os.Stat(targetAbs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", "", fmt.Errorf("cache directory not found: %s", cacheDir)
		}
		return "", "", "", fmt.Errorf("stat failed: %w", err)
	}
	if !stat.IsDir() {
		return "", "", "", errors.New("cacheDir is not a directory")
	}

	modelID := strings.TrimSpace(decodeHFModelIDFromCacheDir(cacheDir))
	if modelID == "" {
		modelID = cacheDir
	}

	return cacheDir, targetAbs, modelID, nil
}

func resolvePreferredHFBackendDir(format string) (string, string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	backendName := defaultHFVLLMBackendName
	if format == "gguf" {
		backendName = defaultHFLLAMACPPBackendName
	}

	if backendDir := strings.TrimSpace(resolveKnownBackendByName(backendName)); backendDir != "" {
		return backendName, backendDir, nil
	}

	fallback := ""
	if home := userHomeDir(); home != "" {
		fallback = filepath.Join(home, "swap-laboratories", "backend", backendName)
	}
	if isRecipeBackendDir(fallback) {
		if abs, err := filepath.Abs(fallback); err == nil {
			fallback = abs
		}
		return backendName, fallback, nil
	}

	if format == "gguf" {
		return "", "", fmt.Errorf("required backend not found for gguf: %s", backendName)
	}
	return "", "", fmt.Errorf("required backend not found for safetensors: %s", backendName)
}

func resolveHFModelSnapshotRoot(modelDir string) (string, bool) {
	modelDir = strings.TrimSpace(modelDir)
	if modelDir == "" {
		return "", false
	}

	mainRefPath := filepath.Join(modelDir, "refs", "main")
	raw, err := os.ReadFile(mainRefPath)
	if err != nil {
		return "", false
	}
	revision := strings.TrimSpace(string(raw))
	if revision == "" {
		return "", false
	}

	snapshotRoot := filepath.Join(modelDir, "snapshots", revision)
	if stat, err := os.Stat(snapshotRoot); err != nil || !stat.IsDir() {
		return "", false
	}
	return snapshotRoot, true
}

func detectHFModelFormat(modelDir, cacheDir, modelID string) (string, string, error) {
	modelDir = strings.TrimSpace(modelDir)
	if modelDir == "" {
		return "", "", errors.New("model directory is empty")
	}

	scanRoot := modelDir
	snapshotRoot, snapshotScoped := resolveHFModelSnapshotRoot(modelDir)
	if snapshotScoped {
		scanRoot = snapshotRoot
	}

	var ggufCount, safetensorsCount int
	largestGGUFPath := ""
	largestGGUFSize := int64(-1)
	largestSafetensorsSize := int64(-1)

	if err := filepath.WalkDir(scanRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		nameLower := strings.ToLower(d.Name())
		size := int64(-1)
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}

		switch {
		case strings.HasSuffix(nameLower, ".gguf"):
			ggufCount++
			if size >= largestGGUFSize {
				largestGGUFSize = size
				largestGGUFPath = path
			}
		case strings.HasSuffix(nameLower, ".safetensors"):
			safetensorsCount++
			if size >= largestSafetensorsSize {
				largestSafetensorsSize = size
			}
		}
		return nil
	}); err != nil {
		return "", "", fmt.Errorf("scan model files failed: %w", err)
	}

	if ggufCount == 0 && safetensorsCount == 0 {
		return "", "", errors.New("no .gguf or .safetensors files found in model directory")
	}

	format := ""
	switch {
	case ggufCount > 0 && safetensorsCount == 0:
		format = "gguf"
	case safetensorsCount > 0 && ggufCount == 0:
		format = "safetensors"
	default:
		identity := strings.ToLower(strings.TrimSpace(cacheDir + " " + modelID))
		if strings.Contains(identity, "gguf") {
			format = "gguf"
		} else if strings.Contains(identity, "safetensor") {
			format = "safetensors"
		} else if largestGGUFSize >= largestSafetensorsSize {
			format = "gguf"
		} else {
			format = "safetensors"
		}
	}

	ggufFileHint := ""
	if format == "gguf" && snapshotScoped && largestGGUFPath != "" {
		if rel, err := filepath.Rel(scanRoot, largestGGUFPath); err == nil {
			rel = strings.TrimSpace(rel)
			if rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
				ggufFileHint = filepath.ToSlash(rel)
			}
		}
	}

	return format, ggufFileHint, nil
}

func sanitizeHFRecipeSlug(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "hf-model"
	}

	var b strings.Builder
	lastDash := false
	for _, ch := range input {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "hf-model"
	}
	return slug
}

func autoGeneratedHFRecipeRef(cacheDir, format string) string {
	suffix := "safetensors"
	if strings.EqualFold(strings.TrimSpace(format), "gguf") {
		suffix = "gguf"
	}
	name := fmt.Sprintf("%s-%s-auto", sanitizeHFRecipeSlug(cacheDir), suffix)
	return filepath.ToSlash(filepath.Join("autogen", name))
}

func buildAutoGeneratedHFRecipeContent(recipeRef, modelID, format, backendName, ggufFile string) ([]byte, error) {
	recipeRef = strings.TrimSpace(recipeRef)
	modelID = strings.TrimSpace(modelID)
	format = strings.ToLower(strings.TrimSpace(format))
	backendName = strings.TrimSpace(backendName)
	if recipeRef == "" {
		return nil, errors.New("recipeRef is required")
	}
	if modelID == "" {
		return nil, errors.New("modelId is required")
	}
	if backendName == "" {
		return nil, errors.New("backend name is required")
	}

	recipe := autoGeneratedHFRecipe{
		RecipeVersion: "1",
		RecipeRef:     recipeRef,
		Name:          fmt.Sprintf("%s (%s auto)", modelID, strings.ToUpper(format)),
		Description:   fmt.Sprintf("Auto-generated from HF cache (%s). Tune values in /ui/#/models.", format),
		Model:         modelID,
		Backend:       backendName,
	}

	switch format {
	case "gguf":
		recipe.Runtime = "llama-cpp"
		recipe.Container = "llama-cpp-spark:last"
		recipe.Defaults = map[string]any{
			"port":         8000,
			"host":         "0.0.0.0",
			"n_gpu_layers": 99,
			"ctx_size":     16384,
		}
		recipe.GGUFFile = strings.TrimSpace(ggufFile)
		recipe.Command = `llama-server \
    -hf {model} \
    --host {host} --port {port} \
    --n-gpu-layers {n_gpu_layers} \
    --ctx-size {ctx_size} \
    --flash-attn on --jinja --no-webui`
	case "safetensors":
		recipe.Runtime = "vllm"
		recipe.Container = "vllm-node:latest"
		recipe.Defaults = map[string]any{
			"port":                   8000,
			"host":                   "0.0.0.0",
			"tensor_parallel":        1,
			"gpu_memory_utilization": 0.70,
			"max_model_len":          32768,
		}
		recipe.Command = `vllm serve {model} \
    --host {host} \
    --port {port} \
    --tensor-parallel-size {tensor_parallel} \
    --distributed-executor-backend ray \
    --gpu-memory-utilization {gpu_memory_utilization} \
    --load-format fastsafetensors \
    --enable-prefix-caching \
    --max-model-len {max_model_len}`
	default:
		return nil, fmt.Errorf("unsupported hf model format: %s", format)
	}

	raw, err := yaml.Marshal(&recipe)
	if err != nil {
		return nil, fmt.Errorf("marshal generated recipe failed: %w", err)
	}

	header := `# Auto-generated by HF Models UI.
# You can fine-tune this recipe from /ui/#/models.

`
	return append([]byte(header), raw...), nil
}

func ensureAutoGeneratedHFRecipeFile(backendDir, recipeRef string, content []byte) (string, bool, error) {
	backendDir = strings.TrimSpace(backendDir)
	recipeRef = strings.Trim(strings.TrimSpace(recipeRef), "/")
	if backendDir == "" {
		return "", false, errors.New("backendDir is required")
	}
	if recipeRef == "" {
		return "", false, errors.New("recipeRef is required")
	}
	if len(content) == 0 {
		return "", false, errors.New("recipe content is empty")
	}

	rel := filepath.FromSlash(recipeRef)
	targetPath := filepath.Join(backendDir, "recipes", rel+".yaml")
	recipesRoot := filepath.Join(backendDir, "recipes")

	recipesAbs, err := filepath.Abs(recipesRoot)
	if err != nil {
		return "", false, fmt.Errorf("resolve recipes root failed: %w", err)
	}
	targetAbs, err := filepath.Abs(targetPath)
	if err != nil {
		return "", false, fmt.Errorf("resolve recipe path failed: %w", err)
	}
	if targetAbs != recipesAbs && !strings.HasPrefix(targetAbs, recipesAbs+string(os.PathSeparator)) {
		return "", false, errors.New("generated recipe path escapes recipes directory")
	}

	if stat, err := os.Stat(targetAbs); err == nil {
		if stat.IsDir() {
			return "", false, fmt.Errorf("recipe path is a directory: %s", targetAbs)
		}
		return targetAbs, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("stat generated recipe failed: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
		return "", false, fmt.Errorf("create recipe directory failed: %w", err)
	}
	if err := os.WriteFile(targetAbs, content, 0o644); err != nil {
		return "", false, fmt.Errorf("write generated recipe failed: %w", err)
	}

	return targetAbs, true, nil
}

func (pm *ProxyManager) generateRecipeBackendHFModel(parentCtx context.Context, cacheDir string) (recipeBackendHFRecipeResponse, error) {
	cacheDir, modelDir, modelID, err := resolveHFModelDirFromCacheDir(cacheDir)
	if err != nil {
		return recipeBackendHFRecipeResponse{}, err
	}

	format, ggufFileHint, err := detectHFModelFormat(modelDir, cacheDir, modelID)
	if err != nil {
		return recipeBackendHFRecipeResponse{}, err
	}

	backendName, backendDir, err := resolvePreferredHFBackendDir(format)
	if err != nil {
		return recipeBackendHFRecipeResponse{}, err
	}

	recipeRef := autoGeneratedHFRecipeRef(cacheDir, format)
	recipeContent, err := buildAutoGeneratedHFRecipeContent(recipeRef, modelID, format, backendName, ggufFileHint)
	if err != nil {
		return recipeBackendHFRecipeResponse{}, err
	}
	recipePath, createdRecipe, err := ensureAutoGeneratedHFRecipeFile(backendDir, recipeRef, recipeContent)
	if err != nil {
		return recipeBackendHFRecipeResponse{}, err
	}

	modelEntryID := cacheDir
	if strings.TrimSpace(modelEntryID) == "" {
		modelEntryID = sanitizeHFRecipeSlug(modelID)
	}
	modelName := strings.TrimSpace(modelID)
	if modelName == "" {
		modelName = modelEntryID
	}

	_, err = pm.upsertRecipeModel(parentCtx, upsertRecipeModelRequest{
		ModelID:        modelEntryID,
		RecipeRef:      recipeRef,
		Name:           modelName,
		Description:    fmt.Sprintf("Auto-generated from downloaded HF model (%s).", format),
		UseModelName:   modelID,
		Mode:           "solo",
		TensorParallel: 1,
		Group:          defaultRecipeGroupName,
	})
	if err != nil {
		return recipeBackendHFRecipeResponse{}, fmt.Errorf("failed to add model entry to config: %w", err)
	}

	action := "reused"
	if createdRecipe {
		action = "created"
	}
	message := fmt.Sprintf("Recipe %s (%s) %s and model %s added to config.yaml.", recipeRef, format, action, modelEntryID)

	return recipeBackendHFRecipeResponse{
		CacheDir:      cacheDir,
		ModelID:       modelID,
		Format:        format,
		BackendDir:    backendDir,
		BackendKind:   detectRecipeBackendKind(backendDir),
		RecipeRef:     recipeRef,
		RecipePath:    recipePath,
		ModelEntryID:  modelEntryID,
		CreatedRecipe: createdRecipe,
		Message:       message,
	}, nil
}

func backendGitRemoteOrigin(backendDir string) string {
	if !backendHasGitRepo(backendDir) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", backendDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func shortRepoLabel(repoURL string) string {
	repoURL = strings.TrimSpace(repoURL)
	repoURL = strings.TrimSuffix(repoURL, ".git")
	repoURL = strings.TrimPrefix(repoURL, "git@github.com:")
	repoURL = strings.TrimPrefix(repoURL, "https://github.com/")
	parts := strings.Split(repoURL, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "/" + parts[1]
	}
	if repoURL == "" {
		return "origin"
	}
	return repoURL
}

func recipeBackendVendor(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "trtllm" {
		return "nvidia"
	}
	if k == "llamacpp" {
		return "ggml"
	}
	return ""
}

func recipeBackendActionsForKind(kind, backendDir, repoURL string) []RecipeBackendActionInfo {
	actions := make([]RecipeBackendActionInfo, 0, 4)
	if backendHasGitRepo(backendDir) {
		repoLabel := shortRepoLabel(repoURL)
		actions = append(actions,
			RecipeBackendActionInfo{Action: "git_pull", Label: fmt.Sprintf("Git Pull (%s)", repoLabel), CommandHint: "git pull --ff-only"},
			RecipeBackendActionInfo{Action: "git_pull_rebase", Label: fmt.Sprintf("Git Pull Rebase (%s)", repoLabel), CommandHint: "git pull --rebase --autostash"},
		)
	}

	if backendHasHFDownloadScript() {
		scriptPath := resolveHFDownloadScriptPath()
		actions = append(actions, RecipeBackendActionInfo{
			Action:      "download_hf_model",
			Label:       "Download HF Model",
			CommandHint: fmt.Sprintf("%s <model> --format <gguf|safetensors> [--quantization Q8_0|4bit] -c --copy-parallel", scriptPath),
		})
	}

	if kind == "nvidia" {
		// NVIDIA actions don't require build-and-copy.sh
		actions = append(actions,
			RecipeBackendActionInfo{
				Action:      "pull_nvidia_image",
				Label:       "Pull NVIDIA Image",
				CommandHint: "docker pull <selected>",
			},
			RecipeBackendActionInfo{
				Action:      "update_nvidia_image",
				Label:       "Update NVIDIA Image",
				CommandHint: "docker pull <selected> + persist as new default",
			},
		)
		return actions
	}

	if kind == "llamacpp" {
		actions = append(actions,
			RecipeBackendActionInfo{
				Action:      "build_llamacpp",
				Label:       "Build llama.cpp (latest)",
				CommandHint: "git fetch tags + checkout latest release + build-llama-cpp-spark.sh + docker save/load to autodiscovered peers",
			},
		)
		return actions
	}

	if !backendHasBuildScript(backendDir) {
		return actions
	}

	if kind == "trtllm" {
		actions = append(actions,
			RecipeBackendActionInfo{
				Action:      "pull_trtllm_image",
				Label:       "Pull TRT-LLM Image",
				CommandHint: "docker pull <selected>",
			},
			RecipeBackendActionInfo{
				Action:      "update_trtllm_image",
				Label:       "Update TRT-LLM Image",
				CommandHint: "docker pull <selected> + persist + copy to peers",
			},
		)
		return actions
	}

	actions = append(actions,
		RecipeBackendActionInfo{Action: "build_vllm", Label: "Build vLLM", CommandHint: "./build-and-copy.sh -t vllm-node -c"},
		RecipeBackendActionInfo{Action: "build_mxfp4", Label: "Build MXFP4", CommandHint: "./build-and-copy.sh -t vllm-node-mxfp4 --exp-mxfp4 -c"},
		RecipeBackendActionInfo{Action: "build_vllm_12_0f", Label: "Build 12.0f", CommandHint: "./build-and-copy.sh -t vllm-node-12.0f --gpu-arch 12.0f -c"},
	)
	return actions
}

func trtllmSourceImageOverridePath(backendDir string) string {
	if strings.TrimSpace(backendDir) == "" {
		return ""
	}
	return filepath.Join(backendDir, trtllmSourceImageOverrideFile)
}

func loadTRTLLMSourceImage(backendDir string) string {
	override := trtllmSourceImageOverridePath(backendDir)
	if override != "" {
		if raw, err := os.ReadFile(override); err == nil {
			if v := strings.TrimSpace(string(raw)); v != "" {
				return v
			}
		}
	}
	return ""
}

func persistTRTLLMSourceImage(backendDir, image string) error {
	override := trtllmSourceImageOverridePath(backendDir)
	if override == "" {
		return nil
	}
	image = strings.TrimSpace(image)
	if image == "" {
		if err := os.Remove(override); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := override + ".tmp"
	if err := os.WriteFile(tmp, []byte(image+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, override)
}

func readDefaultTRTLLMSourceImage(backendDir string) string {
	if envImage := strings.TrimSpace(os.Getenv("LLAMA_SWAP_TRTLLM_SOURCE_IMAGE")); envImage != "" {
		return envImage
	}
	scriptPath := filepath.Join(strings.TrimSpace(backendDir), "build-and-copy.sh")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		return defaultTRTLLMSourceImage
	}
	m := trtllmSourceImageRe.FindStringSubmatch(string(raw))
	if len(m) > 1 {
		if v := strings.TrimSpace(m[1]); v != "" {
			return v
		}
	}
	return defaultTRTLLMSourceImage
}

func resolveTRTLLMSourceImage(backendDir, requested string) string {
	if v := strings.TrimSpace(requested); v != "" {
		return v
	}
	if v := loadTRTLLMSourceImage(backendDir); v != "" {
		return v
	}
	return readDefaultTRTLLMSourceImage(backendDir)
}

type nvcrProxyAuthResponse struct {
	Token string `json:"token"`
}

type nvcrTagsResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func nvcrStatusError(resp *http.Response, context string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("%s failed: %s %s", context, resp.Status, strings.TrimSpace(string(body)))
}

func fetchTRTLLMReleaseTags(ctx context.Context) ([]string, error) {
	authReq, err := http.NewRequestWithContext(ctx, http.MethodGet, nvcrProxyAuthURL, nil)
	if err != nil {
		return nil, err
	}
	authResp, err := http.DefaultClient.Do(authReq)
	if err != nil {
		return nil, err
	}
	defer authResp.Body.Close()
	if err := nvcrStatusError(authResp, "nvcr auth"); err != nil {
		return nil, err
	}
	var auth nvcrProxyAuthResponse
	if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
		return nil, err
	}
	if strings.TrimSpace(auth.Token) == "" {
		return nil, errors.New("nvcr auth returned empty token")
	}

	tagsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, nvcrTagsListURL, nil)
	if err != nil {
		return nil, err
	}
	tagsReq.Header.Set("Authorization", "Bearer "+auth.Token)

	tagsResp, err := http.DefaultClient.Do(tagsReq)
	if err != nil {
		return nil, err
	}
	defer tagsResp.Body.Close()
	if err := nvcrStatusError(tagsResp, "nvcr tags"); err != nil {
		return nil, err
	}

	var payload nvcrTagsResponse
	if err := json.NewDecoder(tagsResp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Tags, nil
}

type trtllmTagVersion struct {
	Major int
	Minor int
	Patch int
	RC    *int
	Post  int
	Raw   string
}

func parseTRTLLMTagVersion(tag string) (trtllmTagVersion, bool) {
	tag = strings.TrimSpace(tag)
	m := trtllmTagVersionRe.FindStringSubmatch(tag)
	if len(m) == 0 {
		return trtllmTagVersion{}, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	var rcPtr *int
	if m[4] != "" {
		rc, _ := strconv.Atoi(m[4])
		rcPtr = &rc
	}
	post := 0
	if m[5] != "" {
		post, _ = strconv.Atoi(m[5])
	}
	return trtllmTagVersion{Major: major, Minor: minor, Patch: patch, RC: rcPtr, Post: post, Raw: tag}, true
}

func compareTRTLLMTagVersion(a, b trtllmTagVersion) int {
	if a.Major != b.Major {
		if a.Major < b.Major {
			return -1
		}
		return 1
	}
	if a.Minor != b.Minor {
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	}
	if a.Patch != b.Patch {
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	}

	if a.RC != nil && b.RC == nil {
		return -1
	}
	if a.RC == nil && b.RC != nil {
		return 1
	}
	if a.RC != nil && b.RC != nil {
		if *a.RC < *b.RC {
			return -1
		}
		if *a.RC > *b.RC {
			return 1
		}
	}
	if a.Post != b.Post {
		if a.Post < b.Post {
			return -1
		}
		return 1
	}
	return 0
}

func latestTRTLLMTag(tags []string) string {
	var best trtllmTagVersion
	hasBest := false
	for _, tag := range tags {
		v, ok := parseTRTLLMTagVersion(tag)
		if !ok {
			continue
		}
		if !hasBest || compareTRTLLMTagVersion(v, best) > 0 {
			best = v
			hasBest = true
		}
	}
	if !hasBest {
		return ""
	}
	return best.Raw
}

func topTRTLLMTags(tags []string, limit int) []string {
	versions := make([]trtllmTagVersion, 0, len(tags))
	for _, tag := range tags {
		v, ok := parseTRTLLMTagVersion(tag)
		if ok {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareTRTLLMTagVersion(versions[i], versions[j]) > 0
	})
	if limit <= 0 || len(versions) < limit {
		limit = len(versions)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, versions[i].Raw)
	}
	return out
}

func tagFromImageRef(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if idx := strings.LastIndex(image, ":"); idx >= 0 && idx < len(image)-1 {
		return image[idx+1:]
	}
	return ""
}

func appendUniqueString(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func buildTRTLLMImageState(backendDir string) *RecipeBackendTRTLLMImage {
	defaultImage := readDefaultTRTLLMSourceImage(backendDir)
	selectedImage := resolveTRTLLMSourceImage(backendDir, "")
	if selectedImage == "" {
		selectedImage = defaultImage
	}
	state := &RecipeBackendTRTLLMImage{
		Selected: selectedImage,
		Default:  defaultImage,
	}
	state.Available = appendUniqueString(state.Available, selectedImage)
	state.Available = appendUniqueString(state.Available, defaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	tags, err := fetchTRTLLMReleaseTags(ctx)
	if err != nil {
		state.Warning = fmt.Sprintf("No se pudieron consultar tags de nvcr.io: %v", err)
		return state
	}

	latestTag := latestTRTLLMTag(tags)
	if latestTag != "" {
		latestImage := "nvcr.io/nvidia/tensorrt-llm/release:" + latestTag
		state.Latest = latestImage
		state.Available = appendUniqueString(state.Available, latestImage)

		selectedTag := tagFromImageRef(selectedImage)
		selectedVersion, selectedOK := parseTRTLLMTagVersion(selectedTag)
		latestVersion, latestOK := parseTRTLLMTagVersion(latestTag)
		if selectedOK && latestOK && compareTRTLLMTagVersion(selectedVersion, latestVersion) < 0 {
			state.UpdateAvailable = true
		}
	}

	for _, tag := range topTRTLLMTags(tags, 12) {
		state.Available = appendUniqueString(state.Available, "nvcr.io/nvidia/tensorrt-llm/release:"+tag)
	}
	return state
}

func uniqueExistingDirs(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = expandLeadingTilde(p)
		abs, err := filepath.Abs(p)
		if err == nil {
			p = abs
		}
		if _, ok := seen[p]; ok {
			continue
		}
		if stat, err := os.Stat(p); err == nil && stat.IsDir() {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

func (pm *ProxyManager) resolveOverrideFilePath(envName, filename string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return expandLeadingTilde(v)
	}
	if pm != nil {
		if cfgPath := strings.TrimSpace(pm.configPath); cfgPath != "" {
			return filepath.Join(filepath.Dir(cfgPath), filename)
		}
	}
	if home := userHomeDir(); home != "" {
		return filepath.Join(home, ".config", "llama-swap", filename)
	}
	return ""
}

func (pm *ProxyManager) recipesBackendOverrideFile() string {
	return pm.resolveOverrideFilePath(recipesBackendOverrideFileEnv, ".recipes_backend_dir")
}

func (pm *ProxyManager) hfHubPathOverrideFile() string {
	return pm.resolveOverrideFilePath(hfHubPathOverrideFileEnv, ".hf_hub_path")
}

func loadOverridePath(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return expandLeadingTilde(strings.TrimSpace(string(raw)))
}

func (pm *ProxyManager) loadRecipesBackendOverride() {
	value := loadOverridePath(pm.recipesBackendOverrideFile())
	if value == "" {
		return
	}
	setRecipesBackendOverride(value)
}

func (pm *ProxyManager) loadHFHubPathOverride() {
	value := loadOverridePath(pm.hfHubPathOverrideFile())
	if value == "" {
		return
	}
	setHFHubPathOverride(value)
}

func (pm *ProxyManager) persistHFHubPathOverride(path string) error {
	filePath := pm.hfHubPathOverrideFile()
	if filePath == "" {
		return nil
	}

	path = strings.TrimSpace(path)
	if path == "" {
		if err := os.Remove(filePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}

	parent := filepath.Dir(filePath)
	if parent != "" {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return err
		}
	}

	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(path+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}

func (pm *ProxyManager) getConfigPath() (string, error) {
	if v := strings.TrimSpace(pm.configPath); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("LLAMA_SWAP_CONFIG_PATH")); v != "" {
		return v, nil
	}
	return "", errors.New("config path is unknown (start llama-swap with --config)")
}

func recipesCatalogPrimaryDir() string {
	dirs := recipesCatalogSearchDirs()
	if len(dirs) == 0 {
		return ""
	}
	return dirs[0]
}

func recipesCatalogSearchDirs() []string {
	dirs := make([]string, 0, 8)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		path = expandLeadingTilde(path)
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if stat, err := os.Stat(path); err == nil && stat.IsDir() {
			dirs = append(dirs, path)
		}
	}

	if v := strings.TrimSpace(os.Getenv(recipesCatalogDirEnv)); v != "" {
		add(v)
	}

	if cfgPath := strings.TrimSpace(os.Getenv("LLAMA_SWAP_CONFIG_PATH")); cfgPath != "" {
		add(filepath.Join(filepath.Dir(expandLeadingTilde(cfgPath)), defaultRecipesCatalogSubdir))
	}

	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, defaultRecipesCatalogSubdir))
	}
	if home := userHomeDir(); home != "" {
		add(filepath.Join(home, "swap-laboratories", defaultRecipesCatalogSubdir))
	}

	backendRoots := make([]string, 0, 4)
	if cfgPath := strings.TrimSpace(os.Getenv("LLAMA_SWAP_CONFIG_PATH")); cfgPath != "" {
		backendRoots = append(backendRoots, filepath.Join(filepath.Dir(expandLeadingTilde(cfgPath)), "backend"))
	}
	if wd, err := os.Getwd(); err == nil {
		backendRoots = append(backendRoots, filepath.Join(wd, "backend"))
	}
	if home := userHomeDir(); home != "" {
		backendRoots = append(backendRoots, filepath.Join(home, "swap-laboratories", "backend"))
	}

	for _, root := range uniqueExistingDirs(backendRoots) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			add(filepath.Join(root, entry.Name(), "recipes"))
		}
	}

	return uniqueExistingDirs(dirs)
}

func isExecutableFile(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return (info.Mode().Perm() & 0o111) != 0
}

func inferBackendDirFromRecipePath(recipePath string) string {
	recipePath = strings.TrimSpace(recipePath)
	if recipePath == "" {
		return ""
	}
	clean := filepath.Clean(recipePath)
	parts := strings.Split(clean, string(os.PathSeparator))
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] != "backend" {
			continue
		}
		candidate := filepath.Join(parts[:i+2]...)
		if isRecipeBackendDir(candidate) {
			return candidate
		}
	}
	return ""
}

func resolveKnownBackendByName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = expandLeadingTilde(name)
	if filepath.IsAbs(name) {
		if isRecipeBackendDir(name) {
			if abs, err := filepath.Abs(name); err == nil {
				return abs
			}
			return filepath.Clean(name)
		}
		return ""
	}

	backendNames := []string{name}
	if !strings.HasPrefix(strings.ToLower(name), "spark-") {
		backendNames = append(backendNames, "spark-"+name)
	}

	roots := make([]string, 0, 4)
	if cfgPath := strings.TrimSpace(os.Getenv("LLAMA_SWAP_CONFIG_PATH")); cfgPath != "" {
		roots = append(roots, filepath.Join(filepath.Dir(expandLeadingTilde(cfgPath)), "backend"))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, filepath.Join(wd, "backend"))
	}
	if home := userHomeDir(); home != "" {
		roots = append(roots, filepath.Join(home, "swap-laboratories", "backend"))
	}

	for _, root := range uniqueExistingDirs(roots) {
		for _, backendName := range backendNames {
			candidate := filepath.Join(root, backendName)
			if isRecipeBackendDir(candidate) {
				if abs, err := filepath.Abs(candidate); err == nil {
					return abs
				}
				return filepath.Clean(candidate)
			}
		}
	}
	return ""
}

func resolveRecipeBackendDirFromMeta(meta recipeCatalogMeta, recipePath, backendDirHint string) string {
	if backend := resolveKnownBackendByName(meta.Backend); backend != "" {
		return backend
	}
	if backend := inferBackendDirFromRecipePath(recipePath); backend != "" {
		return backend
	}

	backendDirHint = strings.TrimSpace(backendDirHint)
	if backendDirHint != "" && isRecipeBackendDir(backendDirHint) {
		if abs, err := filepath.Abs(backendDirHint); err == nil {
			return abs
		}
		return filepath.Clean(backendDirHint)
	}
	return ""
}

func loadRecipeCatalog(backendDirHint string) ([]RecipeCatalogItem, map[string]RecipeCatalogItem, error) {
	searchDirs := recipesCatalogSearchDirs()
	items := make([]RecipeCatalogItem, 0, 16)
	byID := make(map[string]RecipeCatalogItem)
	seenByKey := make(map[string]struct{})

	for _, recipesDir := range searchDirs {
		err := filepath.WalkDir(recipesDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			var meta recipeCatalogMeta
			if err := yaml.Unmarshal(raw, &meta); err != nil {
				return nil
			}

			rel, relErr := filepath.Rel(recipesDir, path)
			if relErr != nil {
				rel = filepath.Base(path)
			}
			rel = strings.TrimSuffix(strings.TrimSuffix(rel, ".yaml"), ".yml")
			rel = filepath.ToSlash(rel)
			id := filepath.Base(rel)
			ref := strings.TrimSpace(meta.RecipeRef)
			if ref == "" {
				ref = rel
			}
			if strings.TrimSpace(ref) == "" {
				ref = id
			}

			key := strings.ToLower(strings.TrimSpace(id))
			if key == "" {
				key = strings.ToLower(strings.TrimSpace(ref))
			}
			if _, exists := seenByKey[key]; exists {
				return nil
			}
			seenByKey[key] = struct{}{}

			defaultTP := intFromAny(meta.Defaults["tensor_parallel"])
			if defaultTP <= 0 {
				defaultTP = 1
			}
			defaultExtraArgs := strings.TrimSpace(stringFromAny(meta.Defaults["extra_args"]))
			if defaultExtraArgs == "" {
				defaultExtraArgs = strings.TrimSpace(stringFromAny(meta.Defaults["extraArgs"]))
			}

			backendDir := resolveRecipeBackendDirFromMeta(meta, path, backendDirHint)
			item := RecipeCatalogItem{
				ID:                    id,
				Ref:                   ref,
				Path:                  path,
				BackendDir:            backendDir,
				BackendKind:           detectRecipeBackendKind(backendDir),
				Name:                  strings.TrimSpace(meta.Name),
				Description:           strings.TrimSpace(meta.Description),
				Model:                 strings.TrimSpace(meta.Model),
				SoloOnly:              meta.SoloOnly,
				ClusterOnly:           meta.ClusterOnly,
				DefaultTensorParallel: defaultTP,
				DefaultExtraArgs:      defaultExtraArgs,
				ContainerImage:        strings.TrimSpace(meta.Container),
			}
			items = append(items, item)

			byID[item.ID] = item
			byID[item.Ref] = item
			byID[path] = item
			byID[filepath.Clean(path)] = item
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, byID, nil
}

func resolveRecipeRef(recipeRef string, catalogByID map[string]RecipeCatalogItem) (string, RecipeCatalogItem, error) {
	trimmed := strings.TrimSpace(recipeRef)
	if trimmed == "" {
		return "", RecipeCatalogItem{}, errors.New("recipeRef is required")
	}
	if item, ok := catalogByID[trimmed]; ok {
		return item.Ref, item, nil
	}

	base := filepath.Base(trimmed)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
	if item, ok := catalogByID[base]; ok {
		return item.Ref, item, nil
	}

	if stat, err := os.Stat(trimmed); err == nil && !stat.IsDir() {
		abs := trimmed
		if resolvedAbs, absErr := filepath.Abs(trimmed); absErr == nil {
			abs = resolvedAbs
		}
		baseID := filepath.Base(strings.TrimSuffix(strings.TrimSuffix(abs, ".yaml"), ".yml"))
		item := RecipeCatalogItem{
			ID:          baseID,
			Ref:         baseID,
			Path:        abs,
			BackendDir:  resolveRecipeBackendDirFromMeta(recipeCatalogMeta{}, abs, ""),
			BackendKind: detectRecipeBackendKind(resolveRecipeBackendDirFromMeta(recipeCatalogMeta{}, abs, "")),
			Name:        filepath.Base(abs),
		}
		return item.Ref, item, nil
	}

	return "", RecipeCatalogItem{}, fmt.Errorf("recipeRef not found: %s", recipeRef)
}

func resolveCatalogRecipeItem(recipeRef string) (RecipeCatalogItem, error) {
	catalog, catalogByID, err := loadRecipeCatalog("")
	if err != nil {
		return RecipeCatalogItem{}, err
	}
	if len(catalog) == 0 {
		return RecipeCatalogItem{}, errors.New("no recipes found in catalog")
	}

	trimmed := strings.TrimSpace(recipeRef)
	if trimmed == "" {
		return catalog[0], nil
	}
	if item, ok := catalogByID[trimmed]; ok {
		return item, nil
	}

	normalized := filepath.Clean(trimmed)
	for _, item := range catalog {
		if strings.TrimSpace(item.Ref) == trimmed || strings.TrimSpace(item.Path) == trimmed {
			return item, nil
		}
		if filepath.Clean(item.Path) == normalized {
			return item, nil
		}
	}

	return RecipeCatalogItem{}, fmt.Errorf("recipeRef not found in catalog: %s", recipeRef)
}

func (pm *ProxyManager) readRecipeSourceState(recipeRef string) (recipeSourceState, error) {
	item, err := resolveCatalogRecipeItem(recipeRef)
	if err != nil {
		return recipeSourceState{}, err
	}

	raw, err := os.ReadFile(item.Path)
	if err != nil {
		return recipeSourceState{}, fmt.Errorf("failed to read recipe source: %w", err)
	}

	state := recipeSourceState{
		RecipeID:  item.ID,
		RecipeRef: item.Ref,
		Path:      item.Path,
		Content:   string(raw),
	}
	if info, err := os.Stat(item.Path); err == nil {
		state.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
	}

	return state, nil
}

func (pm *ProxyManager) saveRecipeSourceState(recipeRef string, content string) (recipeSourceState, error) {
	item, err := resolveCatalogRecipeItem(recipeRef)
	if err != nil {
		return recipeSourceState{}, err
	}

	if err := writeRawFileAtomic(item.Path, []byte(content)); err != nil {
		return recipeSourceState{}, fmt.Errorf("failed to write recipe source: %w", err)
	}

	return pm.readRecipeSourceState(item.Ref)
}

func (pm *ProxyManager) createRecipeSourceState(recipeRef string, content string, overwrite bool) (recipeSourceState, error) {
	recipeRef = strings.TrimSpace(recipeRef)
	if recipeRef == "" {
		return recipeSourceState{}, errors.New("recipeRef is required")
	}

	if strings.Contains(recipeRef, "..") {
		return recipeSourceState{}, errors.New("recipeRef cannot contain '..'")
	}

	refPath := filepath.ToSlash(strings.Trim(strings.TrimSpace(recipeRef), "/"))
	if refPath == "" {
		return recipeSourceState{}, errors.New("recipeRef is required")
	}

	primaryDir := strings.TrimSpace(recipesCatalogPrimaryDir())
	if primaryDir == "" {
		return recipeSourceState{}, errors.New("recipes catalog directory not found")
	}
	primaryDir = filepath.Clean(primaryDir)

	relPath := filepath.Clean(filepath.FromSlash(refPath))
	if relPath == "." || relPath == string(os.PathSeparator) {
		return recipeSourceState{}, errors.New("invalid recipeRef")
	}
	if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		return recipeSourceState{}, errors.New("recipeRef must be relative to recipes dir")
	}

	targetPath := filepath.Join(primaryDir, relPath)
	ext := strings.ToLower(filepath.Ext(targetPath))
	if ext != ".yaml" && ext != ".yml" {
		targetPath += ".yaml"
	}
	targetPath = filepath.Clean(targetPath)

	prefix := primaryDir + string(os.PathSeparator)
	if targetPath != primaryDir && !strings.HasPrefix(targetPath, prefix) {
		return recipeSourceState{}, errors.New("recipe path escapes recipes directory")
	}

	if info, err := os.Stat(targetPath); err == nil {
		if info.IsDir() {
			return recipeSourceState{}, fmt.Errorf("recipe path is a directory: %s", targetPath)
		}
		if !overwrite {
			return recipeSourceState{}, fmt.Errorf("recipe already exists: %s", filepath.Base(targetPath))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return recipeSourceState{}, err
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return recipeSourceState{}, err
	}
	if err := writeRawFileAtomic(targetPath, []byte(content)); err != nil {
		return recipeSourceState{}, fmt.Errorf("failed to create recipe source: %w", err)
	}

	refNoExt := strings.TrimSuffix(filepath.ToSlash(relPath), filepath.Ext(relPath))
	if refNoExt == "" {
		refNoExt = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(targetPath), ".yaml"), ".yml")
	}

	if state, err := pm.readRecipeSourceState(refNoExt); err == nil {
		return state, nil
	}

	state := recipeSourceState{
		RecipeID:  strings.TrimSuffix(strings.TrimSuffix(filepath.Base(targetPath), ".yaml"), ".yml"),
		RecipeRef: refNoExt,
		Path:      targetPath,
		Content:   content,
	}
	if info, err := os.Stat(targetPath); err == nil {
		state.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	return state, nil
}

func writeRawFileAtomic(path string, raw []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadConfigRawMap(configPath string) (map[string]any, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	normalized := normalizeYAMLValue(parsed)
	root, ok := normalized.(map[string]any)
	if !ok || root == nil {
		return map[string]any{}, nil
	}
	return root, nil
}

func writeConfigRawMap(configPath string, root map[string]any) error {
	rendered, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	if _, err := config.LoadConfigFromReader(bytes.NewReader(rendered)); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}

	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, rendered, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}

func normalizeYAMLValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = normalizeYAMLValue(vv)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[fmt.Sprintf("%v", k)] = normalizeYAMLValue(vv)
		}
		return m
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return v
	}
}

func getMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return map[string]any{}
	}
	if key == "" {
		return parent
	}
	if raw, ok := parent[key]; ok {
		if m, ok := raw.(map[string]any); ok {
			return m
		}
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if raw, ok := m[key]; ok {
		return strings.TrimSpace(fmt.Sprintf("%v", raw))
	}
	return ""
}

func getBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, parsed := parseAnyBool(v)
	return parsed && b
}
func buildVLLMContainerDetectExpr(containerImage string) string {
	containerImage = strings.TrimSpace(containerImage)
	if containerImage != "" {
		return fmt.Sprintf(
			"docker ps --filter %s --format \"{{.Names}}\" | head -n 1",
			strconv.Quote("ancestor="+containerImage),
		)
	}
	return "docker ps --format \"{{.Names}}\t{{.Image}}\" | awk \"BEGIN{IGNORECASE=1} \\$1 ~ /vllm/ || \\$2 ~ /vllm/ {print \\$1; exit}\""
}

func buildVLLMStopExpr(containerImage string) string {
	detector := buildVLLMContainerDetectExpr(containerImage)
	return fmt.Sprintf(
		"VLLM_CONTAINER=\"$(%s)\"; if [ -n \"$VLLM_CONTAINER\" ]; then docker exec \"$VLLM_CONTAINER\" pkill -f \"vllm serve\" >/dev/null 2>&1 || true; fi",
		detector,
	)
}

func backendStopExprWithContainer(kind, containerImage string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "llamacpp":
		// Match current naming (llama_cpp_spark_<safe_ref>_<port>) and legacy
		// naming (llama_cpp_spark_<port>) so Unload works across versions.
		return "LLAMA_CPP_CONTAINER=\"$(docker ps --format \"{{.Names}}\" | grep -E \"^llama_cpp_spark_.*_${PORT}$|^llama_cpp_spark_${PORT}$\" | head -n 1)\"; if [ -n \"$LLAMA_CPP_CONTAINER\" ]; then docker rm -f \"$LLAMA_CPP_CONTAINER\" >/dev/null 2>&1 || true; fi"
	default:
		return buildVLLMStopExpr(containerImage)
	}
}

func buildVLLMClusterResetExpr(backendDir, containerImage string) string {
	backendDir = strings.TrimSpace(backendDir)
	if backendDir == "" {
		return ""
	}

	containerImage = strings.TrimSpace(containerImage)
	if containerImage == "" {
		containerImage = "vllm-node:latest"
	}

	return fmt.Sprintf(
		"if docker ps --format \"{{.Names}}\" | grep -q \"^vllm_node$\"; then if ! docker exec vllm_node ray status >/dev/null 2>&1; then (cd %s && ./launch-cluster.sh -t %s stop >/dev/null 2>&1 || true); fi; fi; ",
		quoteForCommand(backendDir),
		quoteForCommand(containerImage),
	)
}

func legacyRecipeContainerImage(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if recipeRaw, ok := metadata[recipeMetadataKey]; ok {
		if recipeMeta, ok := recipeRaw.(map[string]any); ok {
			if image := strings.TrimSpace(fmt.Sprintf("%v", recipeMeta["container_image"])); image != "" {
				return image
			}
		}
	}
	for _, key := range []string{"container_image", "containerImage"} {
		if image := strings.TrimSpace(fmt.Sprintf("%v", metadata[key])); image != "" {
			return image
		}
	}
	return ""
}

func legacyRecipeBackendKind(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if recipeRaw, ok := metadata[recipeMetadataKey]; ok {
		if recipeMeta, ok := recipeRaw.(map[string]any); ok {
			backendDir := strings.TrimSpace(fmt.Sprintf("%v", recipeMeta["backend_dir"]))
			if backendDir != "" {
				return detectRecipeBackendKind(backendDir)
			}
		}
	}
	return ""
}

func legacyRecipeBackendDir(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if recipeRaw, ok := metadata[recipeMetadataKey]; ok {
		if recipeMeta, ok := recipeRaw.(map[string]any); ok {
			return strings.TrimSpace(fmt.Sprintf("%v", recipeMeta["backend_dir"]))
		}
	}
	return ""
}

func legacyRecipeMode(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if recipeRaw, ok := metadata[recipeMetadataKey]; ok {
		if recipeMeta, ok := recipeRaw.(map[string]any); ok {
			return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", recipeMeta["mode"])))
		}
	}
	return ""
}

func ensureVLLMClusterResetInCommand(cmd, containerImage string, metadata map[string]any) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return cmd
	}
	if legacyRecipeMode(metadata) != "cluster" {
		return cmd
	}
	if !strings.Contains(trimmed, "${recipe_runner}") && !strings.Contains(trimmed, "run-recipe.sh") {
		return cmd
	}
	if strings.Contains(trimmed, "launch-cluster.sh") && strings.Contains(trimmed, " stop") {
		return cmd
	}

	backendDir := legacyRecipeBackendDir(metadata)
	prefix := buildVLLMClusterResetExpr(backendDir, containerImage)
	if prefix == "" {
		return cmd
	}

	const shellPrefix = "bash -lc '"
	if strings.HasPrefix(trimmed, shellPrefix) && strings.HasSuffix(trimmed, "'") {
		inner := strings.TrimSuffix(strings.TrimPrefix(trimmed, shellPrefix), "'")
		if strings.Contains(inner, "launch-cluster.sh") && strings.Contains(inner, " stop") {
			return trimmed
		}
		return shellPrefix + prefix + inner + "'"
	}

	return shellPrefix + prefix + trimmed + "'"
}

func resolveVLLMRuntimeCacheUserHome(conf config.Config) string {
	if raw, ok := conf.Macros.Get("user_home"); ok {
		if home := strings.TrimSpace(fmt.Sprintf("%v", raw)); home != "" {
			if strings.HasSuffix(home, "/") && len(home) > 1 {
				home = strings.TrimRight(home, "/")
			}
			return home
		}
	}
	return "$HOME"
}

func buildVLLMRuntimeCacheExtraDockerArgItems(userHome string) []string {
	home := strings.TrimSpace(userHome)
	if home == "" {
		home = "$HOME"
	}
	if strings.HasSuffix(home, "/") && len(home) > 1 {
		home = strings.TrimRight(home, "/")
	}
	return []string{
		fmt.Sprintf("-v %s/.cache/torchinductor:/tmp/torchinductor_root", home),
		fmt.Sprintf("-v %s/.cache/nv/ComputeCache:/root/.nv/ComputeCache", home),
		"-e TORCHINDUCTOR_CACHE_DIR=/tmp/torchinductor_root",
		"-e CUDA_CACHE_PATH=/root/.nv/ComputeCache",
		"-e TRITON_CACHE_DIR=/root/.triton/cache",
	}
}

func containsRuntimeCacheArgItem(argsValue, item string) bool {
	argsValue = strings.TrimSpace(argsValue)
	item = strings.TrimSpace(item)
	if argsValue == "" || item == "" {
		return false
	}
	re := regexp.MustCompile(`(^|[[:space:]])` + regexp.QuoteMeta(item) + `($|[[:space:]])`)
	return re.MatchString(argsValue)
}

func mergeVLLMRuntimeCacheExtraDockerArgs(existing, userHome string) string {
	merged := strings.TrimSpace(existing)
	for _, item := range buildVLLMRuntimeCacheExtraDockerArgItems(userHome) {
		if containsRuntimeCacheArgItem(merged, item) {
			continue
		}
		if merged == "" {
			merged = item
		} else {
			merged += " " + item
		}
	}
	return strings.TrimSpace(merged)
}

func stripAssignmentValueQuotes(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return raw
	}
	if (raw[0] == '\'' && raw[len(raw)-1] == '\'') || (raw[0] == '"' && raw[len(raw)-1] == '"') {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func injectAssignmentBeforeExec(inner, assignment string) string {
	inner = strings.TrimSpace(inner)
	assignment = strings.TrimSpace(assignment)
	if inner == "" || assignment == "" {
		return inner
	}
	if idx := strings.Index(inner, "exec "); idx >= 0 {
		return inner[:idx] + assignment + " " + inner[idx:]
	}
	return assignment + " " + inner
}

func injectAssignmentBeforeEscapedRemoteExec(cmd, assignment string) (string, bool) {
	marker := "\\\"exec "
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		return cmd, false
	}
	return cmd[:idx+2] + assignment + " " + cmd[idx+2:], true
}

func vllmRuntimeCacheAssignment(value string, escapedRemote bool) string {
	if escapedRemote {
		// Escaped remote commands are embedded inside local bash -lc '...'.
		// Use escaped double-quotes to avoid breaking outer single-quote parsing.
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `VLLM_SPARK_EXTRA_DOCKER_ARGS=\\\"` + escaped + `\\\"`
	}
	// Local bash -lc '<cmd>' commands cannot safely contain nested single quotes.
	return "VLLM_SPARK_EXTRA_DOCKER_ARGS=" + quoteForCommand(value)
}

func ensureVLLMRuntimeCachePolicyInCommand(cmd, userHome string) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return cmd
	}

	wrappedShell := false
	workingCmd := trimmed
	const bashLCPrefix = "bash -lc "
	if strings.HasPrefix(trimmed, bashLCPrefix) {
		wrappedShell = true
		workingCmd = strings.TrimSpace(strings.TrimPrefix(trimmed, bashLCPrefix))
		if args, err := config.SanitizeCommand(trimmed); err == nil && len(args) == 3 && args[0] == "bash" && args[1] == "-lc" {
			workingCmd = strings.TrimSpace(args[2])
		}
	}

	escapedRemote := strings.Contains(workingCmd, "\\\"exec ")
	assignment := ""
	if matched := vllmExtraDockerArgsAssignRe.FindString(workingCmd); matched != "" {
		if eq := strings.Index(matched, "="); eq >= 0 && eq+1 < len(matched) {
			existing := stripAssignmentValueQuotes(matched[eq+1:])
			merged := mergeVLLMRuntimeCacheExtraDockerArgs(existing, userHome)
			assignment = vllmRuntimeCacheAssignment(merged, escapedRemote)
			workingCmd = vllmExtraDockerArgsAssignRe.ReplaceAllString(workingCmd, assignment)
		}
	} else {
		merged := mergeVLLMRuntimeCacheExtraDockerArgs("", userHome)
		assignment = vllmRuntimeCacheAssignment(merged, escapedRemote)

		if escapedRemote {
			if updated, ok := injectAssignmentBeforeEscapedRemoteExec(workingCmd, assignment); ok {
				workingCmd = updated
			} else {
				// Fallback for legacy remote command shapes where the escaped exec marker differs.
				assignment = vllmRuntimeCacheAssignment(merged, false)
				workingCmd = injectAssignmentBeforeExec(workingCmd, assignment)
			}
		} else {
			workingCmd = injectAssignmentBeforeExec(workingCmd, assignment)
		}
	}

	if wrappedShell {
		return "bash -lc " + strconv.Quote(workingCmd)
	}

	return workingCmd
}

func normalizeLegacyVLLMDetectorFilterQuoting(cmd string) string {
	if strings.TrimSpace(cmd) == "" {
		return cmd
	}
	return legacyAncestorFilterSingleQuoteRe.ReplaceAllString(cmd, `--filter "ancestor=$1"`)
}

func normalizeLegacyVLLMCommand(cmd, containerImage string, requireContainer bool) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" || !strings.Contains(trimmed, "vllm_node") {
		return cmd
	}

	rewritten := strings.ReplaceAll(trimmed, "docker exec -i vllm_node", "docker exec -i \"$VLLM_CONTAINER\"")
	rewritten = strings.ReplaceAll(rewritten, "docker exec vllm_node", "docker exec \"$VLLM_CONTAINER\"")
	rewritten = strings.ReplaceAll(rewritten, "docker stop vllm_node", "docker stop \"$VLLM_CONTAINER\"")

	detector := buildVLLMContainerDetectExpr(containerImage)
	guard := "if [ -z \"$VLLM_CONTAINER\" ]; then exit 0; fi; "
	if requireContainer {
		guard = "if [ -z \"$VLLM_CONTAINER\" ]; then echo \"No running vLLM container found\" >&2; exit 1; fi; "
	}
	prefix := fmt.Sprintf("VLLM_CONTAINER=\"$(%s)\"; %s", detector, guard)

	const shellPrefix = "bash -lc '"
	if strings.HasPrefix(rewritten, shellPrefix) && strings.HasSuffix(rewritten, "'") {
		inner := strings.TrimSuffix(strings.TrimPrefix(rewritten, shellPrefix), "'")
		if strings.Contains(inner, "VLLM_CONTAINER=\"$(") {
			return rewritten
		}
		return shellPrefix + prefix + inner + "'"
	}

	if strings.Contains(rewritten, "VLLM_CONTAINER=\"$(") {
		return rewritten
	}
	return shellPrefix + prefix + rewritten + "'"
}

func normalizeLegacyVLLMConfigCommands(conf config.Config) config.Config {
	if len(conf.Models) == 0 {
		return conf
	}

	defaultContainerImage := ""
	if raw, ok := conf.Macros.Get("vllm_container_image"); ok {
		defaultContainerImage = strings.TrimSpace(fmt.Sprintf("%v", raw))
	}
	userHome := resolveVLLMRuntimeCacheUserHome(conf)

	for modelID, modelCfg := range conf.Models {
		containerImage := legacyRecipeContainerImage(modelCfg.Metadata)
		if containerImage == "" {
			containerImage = defaultContainerImage
		}
		backendKind := legacyRecipeBackendKind(modelCfg.Metadata)
		modelCfg.Cmd = normalizeLegacyVLLMCommand(modelCfg.Cmd, containerImage, true)
		modelCfg.CmdStop = normalizeLegacyVLLMCommand(modelCfg.CmdStop, containerImage, false)
		modelCfg.Cmd = normalizeLegacyVLLMDetectorFilterQuoting(modelCfg.Cmd)
		modelCfg.CmdStop = normalizeLegacyVLLMDetectorFilterQuoting(modelCfg.CmdStop)
		if backendKind == "vllm" {
			modelCfg.Cmd = ensureVLLMClusterResetInCommand(modelCfg.Cmd, containerImage, modelCfg.Metadata)
			modelCfg.Cmd = ensureVLLMRuntimeCachePolicyInCommand(modelCfg.Cmd, userHome)
		}
		conf.Models[modelID] = modelCfg
	}
	return conf
}

func hasMacro(root map[string]any, name string) bool {
	macros := getMap(root, "macros")
	_, ok := macros[name]
	return ok
}

func backendMacroExprForKind(root map[string]any, kind string, suffix string) (string, bool) {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return "", false
	}

	candidates := make([]string, 0, 8)
	seen := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		candidates = append(candidates, name)
	}

	switch kind {
	case "trtllm":
		add("trtllm_" + suffix)
		add("vllm_" + suffix)
		add("sqlang_" + suffix)
	case "sqlang":
		add("sqlang_" + suffix)
		add("vllm_" + suffix)
		add("trtllm_" + suffix)
	case "llamacpp":
		add("llamacpp_" + suffix)
	default:
		add("vllm_" + suffix)
		add("trtllm_" + suffix)
		add("sqlang_" + suffix)
	}
	add(suffix)

	for _, name := range candidates {
		if hasMacro(root, name) {
			return "${" + name + "}", true
		}
	}
	return "", false
}

func ensureRecipeMacros(root map[string]any, configPath string) {
	macros := getMap(root, "macros")

	if _, ok := macros["user_home"]; !ok {
		macros["user_home"] = "${env.HOME}"
	}

	cfgPath := strings.TrimSpace(configPath)
	if cfgPath != "" {
		llamaRoot := filepath.Dir(cfgPath)
		if abs, err := filepath.Abs(llamaRoot); err == nil {
			llamaRoot = abs
		}
		macros["spark_root"] = llamaRoot
		dispatchRunner := filepath.Join(llamaRoot, "run-recipe.sh")
		if isExecutableFile(dispatchRunner) {
			macros["recipe_runner"] = dispatchRunner
		}
		macros["llama_root"] = llamaRoot
	} else {
		if _, ok := macros["llama_root"]; !ok {
			macros["llama_root"] = "${user_home}/llama-swap"
		}
		if _, ok := macros["spark_root"]; !ok {
			macros["spark_root"] = "${llama_root}"
		}
		if _, ok := macros["recipe_runner"]; !ok {
			macros["recipe_runner"] = "${spark_root}/run-recipe.sh"
		}
	}
	if _, ok := macros["llama_root"]; !ok {
		macros["llama_root"] = "${user_home}/llama-swap"
	}
	if _, ok := macros["spark_root"]; !ok {
		macros["spark_root"] = "${llama_root}"
	}
	if _, ok := macros["recipe_runner"]; !ok {
		macros["recipe_runner"] = "${spark_root}/run-recipe.sh"
	}

	root["macros"] = macros
}

func groupMembers(group map[string]any) []string {
	raw, ok := group["members"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s := strings.TrimSpace(fmt.Sprintf("%v", item))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func removeModelFromAllGroups(groups map[string]any, modelID string) {
	for groupName, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		members := groupMembers(group)
		filtered := make([]string, 0, len(members))
		for _, m := range members {
			if m != modelID {
				filtered = append(filtered, m)
			}
		}
		group["members"] = toAnySlice(filtered)
		groups[groupName] = group
	}
}

func sortedGroupNames(groups map[string]any) []string {
	names := make([]string, 0, len(groups))
	for groupName := range groups {
		names = append(names, groupName)
	}
	sort.Strings(names)
	return names
}

func toAnySlice(items []string) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func uniqueStrings(items []string) []any {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		s := strings.TrimSpace(item)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return toAnySlice(out)
}

func canonicalRecipeBackendDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = expandLeadingTilde(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func recipeEntryTargetsActiveBackend(metadata map[string]any, activeBackendDir string) bool {
	active := canonicalRecipeBackendDir(activeBackendDir)
	if active == "" {
		return true
	}
	recipeMeta := getMap(metadata, recipeMetadataKey)
	backendDir := canonicalRecipeBackendDir(getString(recipeMeta, "backend_dir"))
	if backendDir == "" {
		return true
	}
	return backendDir == active
}

func recipeManagedModelInCatalog(model RecipeManagedModel, catalogByID map[string]RecipeCatalogItem) bool {
	if len(catalogByID) == 0 {
		return true
	}
	ref := strings.TrimSpace(model.RecipeRef)
	if ref == "" {
		return true
	}
	_, ok := catalogByID[ref]
	return ok
}

func cleanAliases(aliases []string) []string {
	seen := make(map[string]struct{}, len(aliases))
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		s := strings.TrimSpace(alias)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sanitizeGroupSuffix(value string) string {
	v := strings.TrimSpace(strings.ToLower(value))
	if v == "" {
		return "node"
	}
	// Keep names config-friendly while retaining host/IP readability.
	v = strings.ReplaceAll(v, ".", "-")
	v = strings.ReplaceAll(v, ":", "-")
	v = strings.ReplaceAll(v, "/", "-")
	v = strings.ReplaceAll(v, "\\", "-")
	v = strings.ReplaceAll(v, " ", "-")
	v = strings.Trim(v, "-")
	if v == "" {
		return "node"
	}
	return v
}

func toRecipeManagedModel(modelID string, modelMap, groupsMap map[string]any) (RecipeManagedModel, bool) {
	cmd := getString(modelMap, "cmd")
	metadata := getMap(modelMap, "metadata")
	recipeMeta := getMap(metadata, recipeMetadataKey)
	managed := getBool(recipeMeta, recipeMetadataManagedField)

	isRecipeModel := recipeRunnerRe.MatchString(cmd)
	if !managed && !isRecipeModel {
		return RecipeManagedModel{}, false
	}

	recipeRef := getString(recipeMeta, "recipe_ref")
	if recipeRef == "" && cmd != "" {
		if m := recipeRunnerRe.FindStringSubmatch(cmd); len(m) > 1 {
			recipeRef = strings.TrimSpace(m[1])
		}
	}

	mode := getString(recipeMeta, "mode")
	if mode == "" {
		if strings.Contains(cmd, "--solo") {
			mode = "solo"
		} else {
			mode = "cluster"
		}
	}

	tp := intFromAny(recipeMeta["tensor_parallel"])
	if tp <= 0 && cmd != "" {
		if m := recipeTpRe.FindStringSubmatch(cmd); len(m) > 1 {
			tp, _ = strconv.Atoi(m[1])
		}
	}
	if tp <= 0 {
		tp = 1
	}

	nodes := getString(recipeMeta, "nodes")
	if nodes == "" && cmd != "" {
		if m := recipeNodesRe.FindStringSubmatch(cmd); len(m) > 1 {
			nodes = strings.Trim(m[1], `"`)
		}
	}

	groupName := getString(recipeMeta, "group")
	if groupName == "" {
		groupName = findModelGroup(modelID, groupsMap)
	}
	if groupName == "" {
		groupName = defaultRecipeGroupName
	}

	aliases := make([]string, 0)
	if rawAliases, ok := modelMap["aliases"].([]any); ok {
		for _, a := range rawAliases {
			s := strings.TrimSpace(fmt.Sprintf("%v", a))
			if s != "" {
				aliases = append(aliases, s)
			}
		}
	}

	var benchyTrustRemoteCode *bool
	if benchy := getMap(metadata, "benchy"); len(benchy) > 0 {
		if v, ok := benchy["trust_remote_code"]; ok {
			if parsed, ok := parseAnyBool(v); ok {
				benchyTrustRemoteCode = &parsed
			}
		}
	}

	return RecipeManagedModel{
		ModelID:               modelID,
		RecipeRef:             recipeRef,
		Name:                  getString(modelMap, "name"),
		Description:           getString(modelMap, "description"),
		Aliases:               aliases,
		UseModelName:          getString(modelMap, "useModelName"),
		Mode:                  mode,
		TensorParallel:        tp,
		Nodes:                 nodes,
		ExtraArgs:             getString(recipeMeta, "extra_args"),
		ContainerImage:        getString(recipeMeta, "container_image"),
		Group:                 groupName,
		Unlisted:              getBool(modelMap, "unlisted"),
		Managed:               managed,
		BenchyTrustRemoteCode: benchyTrustRemoteCode,
		NonPrivileged:         getBool(recipeMeta, "non_privileged"),
		MemLimitGb:            intFromAny(recipeMeta["mem_limit_gb"]),
		MemSwapLimitGb:        intFromAny(recipeMeta["mem_swap_limit_gb"]),
		PidsLimit:             intFromAny(recipeMeta["pids_limit"]),
		ShmSizeGb:             intFromAny(recipeMeta["shm_size_gb"]),
	}, true
}

func findModelGroup(modelID string, groups map[string]any) string {
	for groupName, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, member := range groupMembers(group) {
			if member == modelID {
				return groupName
			}
		}
	}
	return ""
}

func intFromAny(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	default:
		return 0
	}
}

func floatFromAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	default:
		return 0
	}
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

type recipeCommandTemplate struct {
	Command  string         `yaml:"command"`
	Defaults map[string]any `yaml:"defaults"`
}

func buildHotSwapVLLMCommand(recipePath string, tp int, extraArgs string) (string, error) {
	recipePath = strings.TrimSpace(recipePath)
	if recipePath == "" {
		return "", fmt.Errorf("missing recipe path for hot swap command")
	}

	raw, err := os.ReadFile(recipePath)
	if err != nil {
		return "", fmt.Errorf("unable to read recipe file %s: %w", recipePath, err)
	}

	var recipe recipeCommandTemplate
	if err := yaml.Unmarshal(raw, &recipe); err != nil {
		return "", fmt.Errorf("unable to parse recipe file %s: %w", recipePath, err)
	}

	template := strings.TrimSpace(recipe.Command)
	if template == "" {
		return "", fmt.Errorf("recipe %s is missing command template", recipePath)
	}

	params := make(map[string]string, len(recipe.Defaults)+3)
	for key, value := range recipe.Defaults {
		params[key] = stringFromAny(value)
	}
	if strings.TrimSpace(params["host"]) == "" {
		params["host"] = "0.0.0.0"
	}
	params["port"] = "${PORT}"
	if tp > 0 {
		params["tensor_parallel"] = strconv.Itoa(tp)
	}

	rendered := recipeTemplateVarRe.ReplaceAllStringFunc(template, func(token string) string {
		match := recipeTemplateVarRe.FindStringSubmatch(token)
		if len(match) != 2 {
			return token
		}
		if value, ok := params[match[1]]; ok && strings.TrimSpace(value) != "" {
			return value
		}
		return token
	})

	parts := strings.Fields(rendered)
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "\\" {
			continue
		}
		filtered = append(filtered, part)
	}

	command := strings.TrimSpace(strings.Join(filtered, " "))
	if command == "" {
		return "", fmt.Errorf("recipe %s rendered an empty hot swap command", recipePath)
	}

	extra := strings.TrimSpace(extraArgs)
	if extra != "" {
		command = strings.TrimSpace(command + " " + extra)
	}

	return command, nil
}

type nodeGPUFitCandidate struct {
	Node        string
	MaxFreeMiB  int
	MaxTotalMiB int
	MarginMiB   int
	Err         error
}

func resolveGPUMemoryUtilization(recipePath, extraArgs string) float64 {
	if v, ok := parseGPUUtilizationFromExtraArgs(extraArgs); ok {
		return v
	}
	if v := defaultRecipeGPUMemoryUtilization(recipePath); v > 0 {
		return v
	}
	return 0.7
}

func parseGPUUtilizationFromExtraArgs(extraArgs string) (float64, bool) {
	extraArgs = strings.TrimSpace(extraArgs)
	if extraArgs == "" {
		return 0, false
	}
	for _, re := range []*regexp.Regexp{gpuMemoryUtilRe, gpuMemoryUtilAltRe} {
		if re == nil {
			continue
		}
		match := re.FindStringSubmatch(extraArgs)
		if len(match) < 2 {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(match[1]), 64)
		if err != nil {
			continue
		}
		if normalized, ok := normalizeGPUUtilization(parsed); ok {
			return normalized, true
		}
	}
	return 0, false
}

func normalizeGPUUtilization(value float64) (float64, bool) {
	if value <= 0 {
		return 0, false
	}
	if value > 1 && value <= 100 {
		value = value / 100
	}
	if value <= 0 || value > 1 {
		return 0, false
	}
	return value, true
}

func defaultRecipeGPUMemoryUtilization(recipePath string) float64 {
	recipePath = strings.TrimSpace(recipePath)
	if recipePath == "" {
		return 0
	}
	raw, err := os.ReadFile(recipePath)
	if err != nil {
		return 0
	}
	var recipe recipeCommandTemplate
	if err := yaml.Unmarshal(raw, &recipe); err != nil {
		return 0
	}
	parsed := floatFromAny(recipe.Defaults["gpu_memory_utilization"])
	if normalized, ok := normalizeGPUUtilization(parsed); ok {
		return normalized
	}
	return 0
}

func selectBestFitNode(parentCtx context.Context, gpuUtil float64) (string, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 20*time.Second)
	defer cancel()

	nodes, localIP, err := discoverClusterNodeIPs(ctx)
	if err != nil {
		return "", err
	}
	return selectBestFitNodeFromList(ctx, nodes, localIP, gpuUtil)
}

func selectBestFitNodeFromList(parentCtx context.Context, nodes []string, localIP string, gpuUtil float64) (string, error) {
	if len(nodes) == 0 {
		return "", fmt.Errorf("auto node selection failed: no nodes available")
	}
	if normalized, ok := normalizeGPUUtilization(gpuUtil); ok {
		gpuUtil = normalized
	} else {
		gpuUtil = 0.7
	}

	localIPs := localIPv4AddressSet()
	results := make([]nodeGPUFitCandidate, len(nodes))

	var wg sync.WaitGroup
	for idx, host := range nodes {
		idx := idx
		host := strings.TrimSpace(host)
		if host == "" {
			continue
		}
		isLocal := host == strings.TrimSpace(localIP) || isKnownLocalIP(localIPs, host)
		wg.Add(1)
		go func() {
			defer wg.Done()
			devices, err := queryNodeGPUMemory(parentCtx, host, isLocal)
			if err != nil {
				results[idx] = nodeGPUFitCandidate{Node: host, Err: err}
				return
			}
			best := nodeGPUFitCandidate{
				Node:      host,
				MarginMiB: -1 << 30,
			}
			for _, dev := range devices {
				required := int(math.Ceil(gpuUtil * float64(dev.TotalMiB)))
				margin := dev.FreeMiB - required
				if margin > best.MarginMiB {
					best.MaxFreeMiB = dev.FreeMiB
					best.MaxTotalMiB = dev.TotalMiB
					best.MarginMiB = margin
				}
			}
			if best.MarginMiB < 0 {
				best.Err = fmt.Errorf("insufficient GPU free memory: max free %d MiB of %d MiB (need %.0f%%)",
					best.MaxFreeMiB,
					best.MaxTotalMiB,
					gpuUtil*100,
				)
			}
			results[idx] = best
		}()
	}
	wg.Wait()

	var best *nodeGPUFitCandidate
	for idx := range results {
		candidate := results[idx]
		if candidate.Node == "" || candidate.Err != nil {
			continue
		}
		if best == nil || candidate.MarginMiB > best.MarginMiB || (candidate.MarginMiB == best.MarginMiB && candidate.MaxFreeMiB > best.MaxFreeMiB) {
			best = &candidate
		}
	}
	if best != nil {
		return best.Node, nil
	}

	errorsList := make([]string, 0, len(results))
	for _, candidate := range results {
		if candidate.Node == "" || candidate.Err == nil {
			continue
		}
		errorsList = append(errorsList, fmt.Sprintf("%s (%v)", candidate.Node, candidate.Err))
	}
	if len(errorsList) == 0 {
		return "", fmt.Errorf("auto node selection failed: no eligible nodes")
	}
	return "", fmt.Errorf("auto node selection failed: %s", strings.Join(errorsList, "; "))
}

func quoteForShellLiteral(s string) string {
	if s == "" {
		return "''"
	}
	// Single-quote shell escaping.
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func quoteForCommand(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return strconv.Quote(s)
	}
	return s
}

func buildRemoteBackendEnsureExpr(node, backendDir string) string {
	node = strings.TrimSpace(node)
	backendDir = strings.TrimSpace(backendDir)
	if node == "" || backendDir == "" {
		return ""
	}

	backendDir = filepath.Clean(backendDir)
	runnerPath := filepath.Join(backendDir, "run-recipe.sh")
	if !isExecutableFile(runnerPath) {
		return ""
	}

	backendParent := filepath.Dir(backendDir)
	backendBase := filepath.Base(backendDir)
	if backendParent == "" || backendParent == "." || backendBase == "" || backendBase == "." {
		return ""
	}

	remoteCheckInner := "test -x " + strconv.Quote(runnerPath)
	remoteCheckCmd := "bash -lc " + strconv.Quote(remoteCheckInner)

	remoteExtractInner := fmt.Sprintf(
		"mkdir -p %s && tar -C %s -xf -",
		strconv.Quote(backendParent),
		strconv.Quote(backendParent),
	)
	remoteExtractCmd := "bash -lc " + strconv.Quote(remoteExtractInner)

	return strings.Join([]string{
		"if ! ssh -o BatchMode=yes -o StrictHostKeyChecking=no ", quoteForCommand(node), " ", strconv.Quote(remoteCheckCmd), " >/dev/null 2>&1; then ",
		"tar -C ", strconv.Quote(backendParent), " -cf - ", strconv.Quote(backendBase),
		" | ssh -o BatchMode=yes -o StrictHostKeyChecking=no ", quoteForCommand(node), " ", strconv.Quote(remoteExtractCmd), " >/dev/null; fi; ",
	}, "")
}

func splitAndNormalizeNodes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		node := strings.TrimSpace(part)
		if node == "" {
			continue
		}
		if _, ok := seen[node]; ok {
			continue
		}
		seen[node] = struct{}{}
		out = append(out, node)
	}
	return out
}

func commandWithArgs(command string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteForCommand(command))
	for _, arg := range args {
		parts = append(parts, quoteForCommand(arg))
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func normalizeHFFormat(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "", "safetensor", "safetensors":
		return "safetensors"
	case "gguf":
		return "gguf"
	default:
		return ""
	}
}

func buildLLAMACPPBuildAndSyncScript(backendDir, autodiscoverPath, image string) string {
	return strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("BACKEND_DIR=%s", shellQuote(strings.TrimSpace(backendDir))),
		fmt.Sprintf("AUTODISCOVER=%s", shellQuote(strings.TrimSpace(autodiscoverPath))),
		fmt.Sprintf("IMAGE=%s", shellQuote(strings.TrimSpace(image))),
		"LLAMA_DIR=\"$BACKEND_DIR/llama.cpp\"",
		"BUILD_SCRIPT=\"$BACKEND_DIR/build-llama-cpp-spark.sh\"",
		"if [ ! -d \"$LLAMA_DIR/.git\" ]; then",
		"  echo \"llama.cpp source not found: $LLAMA_DIR\" >&2",
		"  exit 1",
		"fi",
		"if [ ! -x \"$BUILD_SCRIPT\" ]; then",
		"  echo \"build script not found or not executable: $BUILD_SCRIPT\" >&2",
		"  exit 1",
		"fi",
		"echo \"[llama.cpp] fetching tags\"",
		"git -C \"$LLAMA_DIR\" fetch --tags --force",
		"LATEST_TAG=\"$(git -C \"$LLAMA_DIR\" tag --sort=-v:refname | head -n1)\"",
		"if [ -n \"$LATEST_TAG\" ]; then",
		"  echo \"[llama.cpp] checkout $LATEST_TAG\"",
		"  git -C \"$LLAMA_DIR\" checkout -f \"$LATEST_TAG\"",
		"else",
		"  echo \"[llama.cpp] no tags found, using current branch\"",
		"fi",
		"echo \"[llama.cpp] build image $IMAGE\"",
		"IMAGE_TAG=\"$IMAGE\" \"$BUILD_SCRIPT\"",
		"if [ ! -f \"$AUTODISCOVER\" ]; then",
		"  echo \"autodiscover.sh not found: $AUTODISCOVER\" >&2",
		"  exit 1",
		"fi",
		"source \"$AUTODISCOVER\"",
		"detect_nodes || true",
		"if [ -z \"${NODES_ARG:-}\" ]; then detect_local_ip || true; fi",
		"NODES_RAW=\"${NODES_ARG:-${LOCAL_IP:-}}\"",
		"if [ -z \"$NODES_RAW\" ]; then",
		"  echo \"No nodes detected for llama.cpp image copy\" >&2",
		"  exit 1",
		"fi",
		"IFS=',' read -r -a NODES <<< \"$NODES_RAW\"",
		"LOCAL=\"${LOCAL_IP:-}\"",
		"for node in \"${NODES[@]}\"; do",
		"  node=\"$(echo \"$node\" | xargs)\"",
		"  [ -z \"$node\" ] && continue",
		"  if [ \"$node\" = \"$LOCAL\" ] || [ \"$node\" = \"127.0.0.1\" ] || [ \"$node\" = \"localhost\" ]; then",
		"    echo \"[local] image available: $IMAGE\"",
		"  else",
		"    echo \"[$node] copying image $IMAGE\"",
		"    docker save \"$IMAGE\" | ssh -o BatchMode=yes -o ConnectTimeout=8 -o ConnectionAttempts=2 -o StrictHostKeyChecking=accept-new \"$node\" \"docker load\"",
		"  fi",
		"done",
	}, "\n")
}

func ensureCommandPathIncludesUserLocalBin(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	env := cmd.Env
	if len(env) == 0 {
		env = os.Environ()
	}

	pathValue := ""
	pathIndex := -1
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathValue = strings.TrimSpace(strings.TrimPrefix(kv, "PATH="))
			pathIndex = i
			break
		}
	}
	if pathValue == "" {
		pathValue = strings.TrimSpace(os.Getenv("PATH"))
	}

	if home := userHomeDir(); home != "" {
		localBin := filepath.Join(home, ".local", "bin")
		if pathValue == "" {
			pathValue = localBin
		} else if !strings.Contains(pathValue, localBin) {
			pathValue = localBin + string(os.PathListSeparator) + pathValue
		}
	}

	if pathValue != "" {
		if pathIndex >= 0 {
			env[pathIndex] = "PATH=" + pathValue
		} else {
			env = append(env, "PATH="+pathValue)
		}
		cmd.Env = env
	}
}

func tailString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return "...(truncated)\n" + s[len(s)-max:]
}

func loadSourceImageOverride(backendDir, overrideFileName string) string {
	data, err := os.ReadFile(filepath.Join(backendDir, overrideFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func persistSourceImageOverride(backendDir, overrideFileName, image string, removeIfEmpty, trailingNewline bool) error {
	overrideFile := filepath.Join(backendDir, overrideFileName)
	image = strings.TrimSpace(image)
	if image == "" && removeIfEmpty {
		if err := os.Remove(overrideFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if trailingNewline {
		image += "\n"
	}
	return os.WriteFile(overrideFile, []byte(image), 0644)
}

func loadLLAMACPPSourceImage(backendDir string) string {
	return loadSourceImageOverride(backendDir, llamacppSourceImageOverrideFile)
}

func persistLLAMACPPSourceImage(backendDir, image string) error {
	return persistSourceImageOverride(backendDir, llamacppSourceImageOverrideFile, image, true, true)
}

func readDefaultLLAMACPPSourceImage(backendDir string) string {
	if envImage := strings.TrimSpace(os.Getenv("LLAMA_SWAP_LLAMACPP_SOURCE_IMAGE")); envImage != "" {
		return envImage
	}
	if image := loadLLAMACPPSourceImage(backendDir); image != "" {
		return image
	}
	return defaultLLAMACPPSparkSourceImage
}

func resolveLLAMACPPSourceImage(backendDir, requested string) string {
	if v := strings.TrimSpace(requested); v != "" {
		return v
	}
	return readDefaultLLAMACPPSourceImage(backendDir)
}

func dockerImageExists(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil
}

func buildLLAMACPPImageState(backendDir string) *RecipeBackendLLAMACPPImage {
	defaultImage := defaultLLAMACPPSparkSourceImage
	selectedImage := resolveLLAMACPPSourceImage(backendDir, "")
	if strings.TrimSpace(selectedImage) == "" {
		selectedImage = defaultImage
	}

	state := &RecipeBackendLLAMACPPImage{
		Selected: selectedImage,
		Default:  defaultImage,
	}
	state.Available = appendUniqueString(state.Available, selectedImage)
	state.Available = appendUniqueString(state.Available, defaultImage)

	if !dockerImageExists(selectedImage) {
		state.Warning = fmt.Sprintf("La imagen seleccionada no existe localmente: %s", selectedImage)
	}
	return state
}

// NVIDIA Image Functions

func loadNVIDIASourceImage(backendDir string) string {
	return loadSourceImageOverride(backendDir, nvidiaSourceImageOverrideFile)
}

func persistNVIDIASourceImage(backendDir, image string) error {
	return persistSourceImageOverride(backendDir, nvidiaSourceImageOverrideFile, image, false, false)
}

// isNVIDIANGCImage checks if an image reference is from NVIDIA NGC Catalog
func isNVIDIANGCImage(image string) bool {
	image = strings.TrimSpace(image)
	// Valid NVIDIA NGC images start with "nvcr.io/nvidia/"
	return strings.HasPrefix(image, "nvcr.io/nvidia/")
}

func readDefaultNVIDIASourceImage(backendDir string) string {
	image := loadNVIDIASourceImage(backendDir)
	if image != "" && isNVIDIANGCImage(image) {
		return image
	}
	return defaultNVIDIASourceImage
}

func resolveNVIDIASourceImage(backendDir, requested string) string {
	if requested != "" {
		return requested
	}
	return readDefaultNVIDIASourceImage(backendDir)
}

func fetchNVIDIAReleaseTags(ctx context.Context) ([]string, error) {
	// Known NVIDIA vLLM image versions from NGC Catalog
	// These are the most commonly used versions
	knownVersions := []string{
		"26.01-py3",
		"25.03-py3",
		"25.02-py3",
		"25.01-py3",
		"24.12-py3",
		"24.11-py3",
		"24.10-py3",
		"24.09-py3",
		"24.08-py3",
		"24.07-py3",
		"24.06-py3",
		"24.05-py3",
	}

	tags := make([]string, 0, len(knownVersions))
	for _, version := range knownVersions {
		imageRef := fmt.Sprintf("nvcr.io/nvidia/vllm:%s", version)
		tags = append(tags, imageRef)
	}

	return tags, nil
}

type nvidiaImageVersion struct {
	imageRef string
	major    int
	minor    int
	patch    int
}

func sortedNVIDIAImageVersions(tags []string) []nvidiaImageVersion {
	versions := make([]nvidiaImageVersion, 0, len(tags))
	for _, imageRef := range tags {
		ver := extractNVIDIAVersion(imageRef)
		if ver == "" {
			continue
		}
		var major, minor, patch int
		var numFields int
		n, _ := fmt.Sscanf(ver, "%d.%d.%d-%*s%n", &major, &minor, &patch, &numFields)
		if n < 3 {
			n, _ = fmt.Sscanf(ver, "%d.%d-%*s%n", &major, &minor, &numFields)
		}
		if n >= 2 {
			versions = append(versions, nvidiaImageVersion{
				imageRef: imageRef,
				major:    major,
				minor:    minor,
				patch:    patch,
			})
		}
	}

	sort.Slice(versions, func(i, j int) bool {
		if versions[i].major != versions[j].major {
			return versions[i].major > versions[j].major
		}
		if versions[i].minor != versions[j].minor {
			return versions[i].minor > versions[j].minor
		}
		return versions[i].patch > versions[j].patch
	})
	return versions
}

func latestNVIDIATag(tags []string) string {
	if len(tags) == 0 {
		return ""
	}

	versions := sortedNVIDIAImageVersions(tags)
	if len(versions) == 0 {
		return tags[0]
	}

	return versions[0].imageRef
}

func extractNVIDIAVersion(imageRef string) string {
	// Extract version from image ref like "nvcr.io/nvidia/vllm:26.01-py3"
	parts := strings.Split(imageRef, ":")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func topNVIDIATags(tags []string, limit int) []string {
	if len(tags) <= limit {
		return tags
	}

	versions := sortedNVIDIAImageVersions(tags)
	if len(versions) == 0 {
		return tags[:limit]
	}

	result := make([]string, 0, limit)
	for i := 0; i < limit && i < len(versions); i++ {
		result = append(result, versions[i].imageRef)
	}
	if len(result) == 0 {
		return tags[:limit]
	}
	return result
}

func buildNVIDIAImageState(backendDir string) *RecipeBackendNVIDIAImage {
	// Default is always the official NVIDIA NGC image
	defaultImage := defaultNVIDIASourceImage

	// Try to load selected image from file, but validate it's an NVIDIA image
	selectedImage := loadNVIDIASourceImage(backendDir)
	if selectedImage == "" || !isNVIDIANGCImage(selectedImage) {
		// If file doesn't exist or contains invalid image, use default
		selectedImage = defaultImage
	}

	state := &RecipeBackendNVIDIAImage{
		Selected: selectedImage,
		Default:  defaultImage,
	}
	state.Available = appendUniqueString(state.Available, selectedImage)
	state.Available = appendUniqueString(state.Available, defaultImage)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	tags, err := fetchNVIDIAReleaseTags(ctx)
	if err != nil {
		state.Warning = fmt.Sprintf("No se pudieron consultar tags de NGC: %v", err)
		return state
	}

	latestImage := latestNVIDIATag(tags)
	if latestImage != "" {
		state.Latest = latestImage
		state.Available = appendUniqueString(state.Available, latestImage)

		selectedVersion := extractNVIDIAVersion(selectedImage)
		latestVersion := extractNVIDIAVersion(latestImage)
		if selectedVersion != "" && latestVersion != "" && selectedVersion != latestVersion {
			state.UpdateAvailable = true
		}
	}

	for _, tag := range topNVIDIATags(tags, 12) {
		state.Available = appendUniqueString(state.Available, tag)
	}
	return state
}

func getDockerContainers() ([]string, error) {
	// Read local docker images and expose relevant tags for recipe container selection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "images", "--format", "{{.Repository}}:{{.Tag}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list docker images: %w", err)
	}

	// Always include common defaults so the UI is stable even if docker has no images yet.
	containers := []string{
		"vllm-next:latest",
		"vllm-node:latest",
		"vllm-node-12.0f:latest",
		"vllm-node-mxfp4:latest",
		"nvcr.io/nvidia/vllm:26.01-py3",
		"avarok/dgx-vllm-nvfp4-kernel:v22",
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	discovered := make([]string, 0, len(lines))
	for _, line := range lines {
		image := strings.TrimSpace(line)
		if image == "" {
			continue
		}
		if strings.Contains(image, "<none>") {
			continue
		}

		lower := strings.ToLower(image)
		if !strings.Contains(lower, "vllm") && !strings.Contains(lower, "llama-cpp") && !strings.Contains(lower, "llamacpp") {
			continue
		}
		discovered = append(discovered, image)
	}
	if len(discovered) > 1 {
		sort.Strings(discovered)
	}

	for _, image := range discovered {
		containers = appendUniqueString(containers, image)
	}

	return containers, nil
}
