package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mostlygeek/llama-swap/event"
)

type Model struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	State          string `json:"state"`
	Unlisted       bool   `json:"unlisted"`
	PeerID         string `json:"peerID"`
	ContainerImage string `json:"containerImage,omitempty"`
	RecipeRef      string `json:"recipeRef,omitempty"`
	Mode           string `json:"mode,omitempty"`
	TensorParallel int    `json:"tensorParallel,omitempty"`
}

func addApiHandlers(pm *ProxyManager) {
	// Add API endpoints for React to consume
	// Protected with API key authentication
	apiGroup := pm.ginEngine.Group("/api", pm.apiKeyAuth())
	{
		apiGroup.POST("/models/unload", pm.apiUnloadAllModels)
		apiGroup.POST("/models/unload/*model", pm.apiUnloadSingleModelHandler)
		apiGroup.POST("/cluster/stop", pm.apiStopCluster)
		apiGroup.GET("/cluster/status", pm.apiGetClusterStatus)
		apiGroup.POST("/cluster/dgx/update", pm.apiRunClusterDGXUpdate)
		apiGroup.GET("/images/docker", pm.apiListDockerImages)
		apiGroup.POST("/images/docker/update", pm.apiUpdateDockerImage)
		apiGroup.POST("/images/docker/delete", pm.apiDeleteDockerImage)
		apiGroup.GET("/config/editor", pm.apiGetConfigEditor)
		apiGroup.PUT("/config/editor", pm.apiSaveConfigEditor)
		apiGroup.GET("/recipes/state", pm.apiGetRecipeState)
		apiGroup.GET("/recipes/backend", pm.apiGetRecipeBackend)
		apiGroup.PUT("/recipes/backend", pm.apiSetRecipeBackend)
		apiGroup.GET("/recipes/containers", pm.apiGetDockerContainers)
		apiGroup.GET("/recipes/selected-container", pm.apiGetSelectedContainer)
		apiGroup.PUT("/recipes/selected-container", pm.apiSetSelectedContainer)
		apiGroup.POST("/recipes/backend/action", pm.apiRunRecipeBackendAction)
		apiGroup.GET("/recipes/backend/action-status", pm.apiGetRecipeBackendActionStatus)
		apiGroup.GET("/recipes/backend/hf-models", pm.apiListRecipeBackendHFModels)
		apiGroup.PUT("/recipes/backend/hf-models/path", pm.apiSetRecipeBackendHFHubPath)
		apiGroup.DELETE("/recipes/backend/hf-models", pm.apiDeleteRecipeBackendHFModel)
		apiGroup.POST("/recipes/backend/hf-models/recipe", pm.apiGenerateRecipeBackendHFModel)
		apiGroup.POST("/recipes/models", pm.apiUpsertRecipeModel)
		apiGroup.DELETE("/recipes/models/:id", pm.apiDeleteRecipeModel)
		apiGroup.GET("/recipes/source", pm.apiGetRecipeSource)
		apiGroup.PUT("/recipes/source", pm.apiSaveRecipeSource)
		apiGroup.POST("/recipes/source/create", pm.apiCreateRecipeSource)
		apiGroup.POST("/benchy", pm.apiStartBenchy)
		apiGroup.GET("/benchy/:id", pm.apiGetBenchyJob)
		apiGroup.POST("/benchy/:id/cancel", pm.apiCancelBenchyJob)
		apiGroup.GET("/events", pm.apiSendEvents)
		apiGroup.GET("/metrics", pm.apiGetMetrics)
		apiGroup.GET("/version", pm.apiGetVersion)
		apiGroup.GET("/captures/:id", pm.apiGetCapture)
	}
}

func (pm *ProxyManager) apiUnloadAllModels(c *gin.Context) {
	pm.StopProcesses(StopImmediately)
	pm.stopVLLMServeFallback()
	pm.stopLLAMACPPServeFallback()
	c.JSON(http.StatusOK, gin.H{"msg": "ok"})
}

func (pm *ProxyManager) stopVLLMServeFallback() {
	// If a vLLM process survived a llama-swap restart and is no longer tied to an
	// in-memory process group, force-stop it without tearing down cluster containers.
	containers, err := detectVLLMFallbackContainers()
	if err != nil {
		pm.proxyLogger.Warnf("fallback container detection failed: %v", err)
	}
	if len(containers) == 0 {
		return
	}
	pm.stopFallbackProcessInContainers(containers, "vllm serve", "vllm serve")
}

func (pm *ProxyManager) stopLLAMACPPServeFallback() {
	// Similar to vLLM fallback handling: ensure orphan llama.cpp servers do not
	// survive across proxy restarts and block clean model unloads/reloads.
	pm.stopFallbackProcessOnHost("llama-server", "llama.cpp server")
}

func (pm *ProxyManager) stopFallbackProcessOnHost(processPattern, processLabel string) {
	cmd := exec.Command("pkill", "-f", processPattern)
	output, execErr := cmd.CombinedOutput()
	if execErr != nil {
		if exitErr, ok := execErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return
		}
		pm.proxyLogger.Warnf(
			"fallback stop of %s failed err=%v output=%s",
			processLabel,
			execErr,
			strings.TrimSpace(string(output)),
		)
		return
	}

	if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
		pm.proxyLogger.Infof("fallback stop of %s output=%s", processLabel, trimmed)
	}
}

func (pm *ProxyManager) stopFallbackProcessInContainers(containers []string, processPattern, processLabel string) {
	seen := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		container = strings.TrimSpace(container)
		if container == "" {
			continue
		}
		if _, ok := seen[container]; ok {
			continue
		}
		seen[container] = struct{}{}

		cmd := exec.Command("docker", "exec", container, "pkill", "-f", processPattern)
		output, execErr := cmd.CombinedOutput()
		if execErr != nil {
			if exitErr, ok := execErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			pm.proxyLogger.Warnf("fallback stop of %s failed container=%s err=%v output=%s", processLabel, container, execErr, strings.TrimSpace(string(output)))
			continue
		}

		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			pm.proxyLogger.Infof("fallback stop of %s container=%s output=%s", processLabel, container, trimmed)
		}
	}
}

func detectVLLMFallbackContainers() ([]string, error) {
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Image}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker ps failed: %w", err)
	}
	return parseFallbackContainersFromDockerPS(string(output), "vllm"), nil
}

func parseFallbackContainersFromDockerPS(output, matchToken string) []string {
	matchToken = strings.ToLower(strings.TrimSpace(matchToken))
	if matchToken == "" {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	containers := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}

		image := ""
		if len(parts) > 1 {
			image = strings.ToLower(strings.TrimSpace(parts[1]))
		}
		if strings.Contains(strings.ToLower(name), matchToken) || strings.Contains(image, matchToken) {
			containers = append(containers, name)
		}
	}

	return containers
}

func normalizeContainerImage(value any) string {
	container := strings.TrimSpace(fmt.Sprintf("%v", value))
	if container == "" || container == "<nil>" {
		return ""
	}
	return container
}

func catalogContainerImage(catalogByID map[string]RecipeCatalogItem, recipeRef string) string {
	recipeRef = strings.TrimSpace(recipeRef)
	if recipeRef == "" || len(catalogByID) == 0 {
		return ""
	}

	candidates := []string{recipeRef}
	if slash := strings.LastIndex(recipeRef, "/"); slash >= 0 {
		candidates = append(candidates, recipeRef[slash+1:])
	}

	seen := make(map[string]struct{}, len(candidates)*2)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, key := range []string{candidate, strings.TrimSuffix(strings.TrimSuffix(candidate, ".yaml"), ".yml")} {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if item, ok := catalogByID[key]; ok {
				if container := normalizeContainerImage(item.ContainerImage); container != "" {
					return container
				}
			}
		}
	}

	return ""
}

func resolveModelContainerImage(modelID, cmd string, metadata map[string]any, catalogByID map[string]RecipeCatalogItem, defaultContainerImage string) string {
	if len(metadata) > 0 {
		if recipeRaw, ok := metadata["recipe_ui"]; ok {
			if recipeMeta, ok := recipeRaw.(map[string]any); ok {
				if container := normalizeContainerImage(recipeMeta["container_image"]); container != "" {
					return container
				}
			}
		}

		for _, key := range []string{"container_image", "containerImage"} {
			if container := normalizeContainerImage(metadata[key]); container != "" {
				return container
			}
		}
	}

	recipeRef := ""
	if len(metadata) > 0 {
		if recipeRaw, ok := metadata["recipe_ui"]; ok {
			if recipeMeta, ok := recipeRaw.(map[string]any); ok {
				recipeRef = strings.TrimSpace(fmt.Sprintf("%v", recipeMeta["recipe_ref"]))
			}
		}
	}
	if recipeRef == "" {
		if matches := recipeRunnerRe.FindStringSubmatch(strings.TrimSpace(cmd)); len(matches) > 1 {
			recipeRef = strings.TrimSpace(matches[1])
		}
	}

	if container := catalogContainerImage(catalogByID, recipeRef); container != "" {
		return container
	}
	if container := catalogContainerImage(catalogByID, modelID); container != "" {
		return container
	}

	if container := normalizeContainerImage(defaultContainerImage); container != "" {
		return container
	}

	return ""
}

func (pm *ProxyManager) getModelStatus() []Model {
	// Extract keys and sort them
	models := []Model{}

	_, catalogByID, catalogErr := loadRecipeCatalog("")
	if catalogErr != nil {
		catalogByID = nil
	}
	defaultContainerImage := ""
	if container, ok := pm.config.Macros.Get("vllm_container_image"); ok {
		defaultContainerImage = normalizeContainerImage(container)
	}

	type probeCandidate struct {
		index   int
		proxies []string
		keys    []string
	}
	candidates := make([]probeCandidate, 0, len(pm.config.Models))

	modelIDs := make([]string, 0, len(pm.config.Models))
	for modelID := range pm.config.Models {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)

	// Iterate over sorted keys
	for _, modelID := range modelIDs {
		modelCfg := pm.config.Models[modelID]

		recipeModel, isRecipe := toRecipeManagedModel(modelID, map[string]any{
			"cmd":      modelCfg.Cmd,
			"metadata": modelCfg.Metadata,
		}, nil)
		if isRecipe && !recipeManagedModelInCatalog(recipeModel, catalogByID) {
			continue
		}

		// Get process state
		state := string(StateStopped)
		probeProxies := []string{modelCfg.Proxy}
		processGroup := pm.findGroupByModelName(modelID)
		if processGroup != nil {
			processGroup.Lock()
			process := processGroup.processes[modelID]
			if process != nil {
				state = string(process.CurrentState())
				if proxy := strings.TrimSpace(process.config.Proxy); proxy != "" {
					probeProxies = append(probeProxies, proxy)
				}
			}
			processGroup.Unlock()
		}

		modelStatus := Model{
			Id:             modelID,
			Name:           modelCfg.Name,
			Description:    modelCfg.Description,
			State:          state,
			Unlisted:       modelCfg.Unlisted,
			ContainerImage: resolveModelContainerImage(modelID, modelCfg.Cmd, modelCfg.Metadata, catalogByID, defaultContainerImage),
		}
		if isRecipe {
			modelStatus.RecipeRef = recipeModel.RecipeRef
			modelStatus.Mode = recipeModel.Mode
			modelStatus.TensorParallel = recipeModel.TensorParallel
		}

		idx := len(models)
		models = append(models, modelStatus)

		if state == string(StateStopped) {
			seenProxies := make(map[string]struct{}, len(probeProxies))
			uniqueProxies := make([]string, 0, len(probeProxies))
			for _, candidateProxy := range probeProxies {
				candidateProxy = strings.TrimSpace(candidateProxy)
				if candidateProxy == "" {
					continue
				}
				if _, exists := seenProxies[candidateProxy]; exists {
					continue
				}
				seenProxies[candidateProxy] = struct{}{}
				uniqueProxies = append(uniqueProxies, candidateProxy)
			}
			candidates = append(candidates, probeCandidate{
				index:   idx,
				proxies: uniqueProxies,
				keys:    []string{modelID, modelCfg.UseModelName},
			})
		}
	}

	// Reconcile with externally running vLLM process(es) after llama-swap restart.
	// This keeps UI status in sync when backend processes survive a proxy restart.
	if len(candidates) > 0 {
		servedByProxy := make(map[string]map[string]struct{})
		for _, candidate := range candidates {
			matched := false
			for _, candidateProxy := range candidate.proxies {
				proxyURL := strings.TrimSpace(candidateProxy)
				if proxyURL == "" {
					continue
				}

				servedIDs, ok := servedByProxy[proxyURL]
				if !ok {
					servedIDs = detectServedModelIDs(proxyURL)
					servedByProxy[proxyURL] = servedIDs
				}
				if len(servedIDs) == 0 {
					continue
				}

				for _, key := range candidate.keys {
					normalized := normalizeModelKey(key)
					if normalized == "" {
						continue
					}
					if _, found := servedIDs[normalized]; found {
						models[candidate.index].State = string(StateReady)
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
		}
	}

	// Iterate over the peer models
	if pm.peerProxy != nil {
		for peerID, peer := range pm.peerProxy.ListPeers() {
			for _, modelID := range peer.Models {
				models = append(models, Model{
					Id:     modelID,
					PeerID: peerID,
				})
			}
		}
	}

	return models
}

func normalizeModelKey(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

func detectServedModelIDs(proxyURL string) map[string]struct{} {
	result := make(map[string]struct{})
	base := strings.TrimRight(strings.TrimSpace(proxyURL), "/")
	if base == "" {
		return result
	}

	client := &http.Client{Timeout: 2 * time.Second}
	endpoints := []string{base + "/v1/models", base + "/models"}

	for _, endpoint := range endpoints {
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			continue
		}

		for _, item := range payload.Data {
			if key := normalizeModelKey(item.ID); key != "" {
				result[key] = struct{}{}
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	return result
}

type messageType string

const (
	msgTypeModelStatus messageType = "modelStatus"
	msgTypeLogData     messageType = "logData"
	msgTypeMetrics     messageType = "metrics"
)

type messageEnvelope struct {
	Type messageType `json:"type"`
	Data string      `json:"data"`
}

// sends a stream of different message types that happen on the server
func (pm *ProxyManager) apiSendEvents(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Content-Type-Options", "nosniff")
	// prevent nginx from buffering SSE
	c.Header("X-Accel-Buffering", "no")

	sendBuffer := make(chan messageEnvelope, 25)
	ctx, cancel := context.WithCancel(c.Request.Context())
	sendModels := func() {
		data, err := json.Marshal(pm.getModelStatus())
		if err == nil {
			msg := messageEnvelope{Type: msgTypeModelStatus, Data: string(data)}
			select {
			case sendBuffer <- msg:
			case <-ctx.Done():
				return
			default:
			}

		}
	}

	sendLogData := func(source string, data []byte) {
		data, err := json.Marshal(gin.H{
			"source": source,
			"data":   string(data),
		})
		if err == nil {
			select {
			case sendBuffer <- messageEnvelope{Type: msgTypeLogData, Data: string(data)}:
			case <-ctx.Done():
				return
			default:
			}
		}
	}

	sendMetrics := func(metrics []TokenMetrics) {
		jsonData, err := json.Marshal(metrics)
		if err == nil {
			select {
			case sendBuffer <- messageEnvelope{Type: msgTypeMetrics, Data: string(jsonData)}:
			case <-ctx.Done():
				return
			default:
			}
		}
	}

	/**
	 * Send updated models list
	 */
	defer event.On(func(e ProcessStateChangeEvent) {
		sendModels()
	})()
	defer event.On(func(e ConfigFileChangedEvent) {
		sendModels()
	})()

	/**
	 * Send Log data
	 */
	defer pm.proxyLogger.OnLogData(func(data []byte) {
		sendLogData("proxy", data)
	})()
	defer pm.upstreamLogger.OnLogData(func(data []byte) {
		sendLogData("upstream", data)
	})()

	/**
	 * Send Metrics data
	 */
	defer event.On(func(e TokenMetricsEvent) {
		sendMetrics([]TokenMetrics{e.Metrics})
	})()

	// send initial batch of data (prioritize model status for faster UI paint)
	sendModels()
	sendMetrics(pm.metricsMonitor.getMetrics())
	sendLogData("proxy", pm.proxyLogger.GetHistory())
	sendLogData("upstream", pm.upstreamLogger.GetHistory())

	for {
		select {
		case <-c.Request.Context().Done():
			cancel()
			return
		case <-pm.shutdownCtx.Done():
			cancel()
			return
		case msg := <-sendBuffer:
			c.SSEvent("message", msg)
			c.Writer.Flush()
		}
	}
}

func (pm *ProxyManager) apiGetMetrics(c *gin.Context) {
	jsonData, err := pm.metricsMonitor.getMetricsJSON()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get metrics"})
		return
	}
	c.Data(http.StatusOK, "application/json", jsonData)
}

func (pm *ProxyManager) apiUnloadSingleModelHandler(c *gin.Context) {
	requestedModel := strings.TrimPrefix(c.Param("model"), "/")
	realModelName, found := pm.config.RealModelName(requestedModel)
	if !found {
		pm.sendErrorResponse(c, http.StatusNotFound, "Model not found")
		return
	}

	processGroup := pm.findGroupByModelName(realModelName)
	if processGroup == nil {
		pm.sendErrorResponse(c, http.StatusInternalServerError, fmt.Sprintf("process group not found for model %s", requestedModel))
		return
	}

	if err := processGroup.StopProcess(realModelName, StopImmediately); err != nil {
		pm.sendErrorResponse(c, http.StatusInternalServerError, fmt.Sprintf("error stopping process: %s", err.Error()))
		return
	} else {
		c.String(http.StatusOK, "OK")
	}
}

func (pm *ProxyManager) apiGetVersion(c *gin.Context) {
	c.JSON(http.StatusOK, map[string]string{
		"version":    pm.version,
		"commit":     pm.commit,
		"build_date": pm.buildDate,
	})
}

func (pm *ProxyManager) apiGetCapture(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid capture ID"})
		return
	}

	capture := pm.metricsMonitor.getCaptureByID(id)
	if capture == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "capture not found"})
		return
	}

	c.JSON(http.StatusOK, capture)
}

func (pm *ProxyManager) apiGetDockerContainers(c *gin.Context) {
	// Get list of vllm-node containers
	containers, err := getDockerContainers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, containers)
}

func (pm *ProxyManager) apiGetSelectedContainer(c *gin.Context) {
	// Get the selected container from config or use default
	selectedContainer := "vllm-node:latest"
	if pm.config.Macros != nil {
		if container, ok := pm.config.Macros.Get("vllm_container_image"); ok {
			if containerStr, ok := container.(string); ok && containerStr != "" {
				selectedContainer = containerStr
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"selectedContainer": selectedContainer})
}

func (pm *ProxyManager) apiSetSelectedContainer(c *gin.Context) {
	var req struct {
		Container string `json:"container" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate container is available
	containers, err := getDockerContainers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	valid := false
	for _, container := range containers {
		if container == req.Container {
			valid = true
			break
		}
	}

	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid container"})
		return
	}

	// TODO: This endpoint is currently disabled to avoid corrupting config.yaml with wrong macro format
	// The container selection is now done per-model via upsertRecipeModel
	// in the ModelsPanel component

	c.JSON(http.StatusOK, gin.H{"selectedContainer": req.Container})
}
