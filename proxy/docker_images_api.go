package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type dockerImageInfo struct {
	Reference    string `json:"reference"`
	Repository   string `json:"repository"`
	Tag          string `json:"tag"`
	ID           string `json:"id"`
	Digest       string `json:"digest,omitempty"`
	Size         string `json:"size"`
	CreatedSince string `json:"createdSince"`
}

type dockerNodeImages struct {
	NodeIP  string            `json:"nodeIp"`
	IsLocal bool              `json:"isLocal"`
	Images  []dockerImageInfo `json:"images"`
	Error   string            `json:"error,omitempty"`
}

type dockerImagesResponse struct {
	Images         []dockerImageInfo  `json:"images"`
	Nodes          []dockerNodeImages `json:"nodes,omitempty"`
	DiscoveryError string             `json:"discoveryError,omitempty"`
}

type dockerImageCLIEntry struct {
	Repository   string `json:"Repository"`
	Tag          string `json:"Tag"`
	ID           string `json:"ID"`
	Digest       string `json:"Digest"`
	CreatedSince string `json:"CreatedSince"`
	Size         string `json:"Size"`
}

type dockerImageActionRequest struct {
	NodeIP    string `json:"nodeIp"`
	Reference string `json:"reference"`
	ID        string `json:"id"`
}

type dockerImageActionResponse struct {
	Action     string `json:"action"`
	NodeIP     string `json:"nodeIp"`
	IsLocal    bool   `json:"isLocal"`
	Command    string `json:"command"`
	Message    string `json:"message"`
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

const dockerImagesListScript = "docker images --no-trunc --format \"{{json .}}\""

func (pm *ProxyManager) apiListDockerImages(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 55*time.Second)
	defer cancel()

	localImages, localErr := dockerImagesForNode(ctx, "127.0.0.1", true, 18*time.Second)
	if localErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": localErr.Error()})
		return
	}

	nodes, localIP, discoverErr := discoverClusterNodeIPs(ctx)
	if discoverErr != nil {
		nodeIP := strings.TrimSpace(localIP)
		if nodeIP == "" {
			nodeIP = "127.0.0.1"
		}
		c.JSON(http.StatusOK, dockerImagesResponse{
			Images: localImages,
			Nodes: []dockerNodeImages{
				{
					NodeIP:  nodeIP,
					IsLocal: true,
					Images:  localImages,
				},
			},
			DiscoveryError: discoverErr.Error(),
		})
		return
	}

	nodes = uniqueNonEmptyStrings(nodes)
	if len(nodes) == 0 {
		nodeIP := strings.TrimSpace(localIP)
		if nodeIP == "" {
			nodeIP = "127.0.0.1"
		}
		c.JSON(http.StatusOK, dockerImagesResponse{
			Images: localImages,
			Nodes: []dockerNodeImages{
				{
					NodeIP:  nodeIP,
					IsLocal: true,
					Images:  localImages,
				},
			},
		})
		return
	}

	localIPs := localIPv4AddressSet()
	results := make([]dockerNodeImages, len(nodes))
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
			result := dockerNodeImages{
				NodeIP:  host,
				IsLocal: isLocal,
			}
			images, err := dockerImagesForNode(ctx, host, isLocal, 20*time.Second)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Images = images
			}
			results[idx] = result
		}()
	}
	wg.Wait()

	results = compactDockerNodeImages(results)
	sortDockerNodeImages(results)

	c.JSON(http.StatusOK, dockerImagesResponse{
		Images: pickLocalDockerImages(results, localImages),
		Nodes:  results,
	})
}

func (pm *ProxyManager) apiUpdateDockerImage(c *gin.Context) {
	req, ok := parseDockerImageActionRequest(c)
	if !ok {
		return
	}

	reference := strings.TrimSpace(req.Reference)
	if !isPullableDockerReference(reference) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reference is required and must be a pullable image (repository:tag)"})
		return
	}

	host, isLocal, err := resolveDockerActionNode(c.Request.Context(), req.NodeIP)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	command := fmt.Sprintf("docker pull %s", shellQuote(reference))
	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 4*time.Minute)
	defer cancel()

	output, runErr := runClusterNodeShell(ctx, host, isLocal, command)
	duration := time.Since(start).Milliseconds()
	nodeLabel := dockerActionNodeLabel(host, isLocal)
	if runErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":      runErr.Error(),
			"action":     "update",
			"nodeIp":     nodeLabel,
			"isLocal":    isLocal,
			"command":    command,
			"durationMs": duration,
			"output":     output,
		})
		return
	}

	c.JSON(http.StatusOK, dockerImageActionResponse{
		Action:     "update",
		NodeIP:     nodeLabel,
		IsLocal:    isLocal,
		Command:    command,
		Message:    fmt.Sprintf("Image updated on %s", nodeLabel),
		Output:     output,
		DurationMs: duration,
	})
}

func (pm *ProxyManager) apiDeleteDockerImage(c *gin.Context) {
	req, ok := parseDockerImageActionRequest(c)
	if !ok {
		return
	}

	target := resolveDockerDeleteTarget(req)
	if target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id or reference is required"})
		return
	}

	host, isLocal, err := resolveDockerActionNode(c.Request.Context(), req.NodeIP)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	command := fmt.Sprintf("docker rmi -f %s", shellQuote(target))
	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	output, runErr := runClusterNodeShell(ctx, host, isLocal, command)
	duration := time.Since(start).Milliseconds()
	nodeLabel := dockerActionNodeLabel(host, isLocal)
	if runErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":      runErr.Error(),
			"action":     "delete",
			"nodeIp":     nodeLabel,
			"isLocal":    isLocal,
			"command":    command,
			"durationMs": duration,
			"output":     output,
		})
		return
	}

	c.JSON(http.StatusOK, dockerImageActionResponse{
		Action:     "delete",
		NodeIP:     nodeLabel,
		IsLocal:    isLocal,
		Command:    command,
		Message:    fmt.Sprintf("Image removed on %s", nodeLabel),
		Output:     output,
		DurationMs: duration,
	})
}

func parseDockerImageActionRequest(c *gin.Context) (dockerImageActionRequest, bool) {
	var req dockerImageActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request body: %v", err)})
		return dockerImageActionRequest{}, false
	}
	req.NodeIP = strings.TrimSpace(req.NodeIP)
	req.Reference = strings.TrimSpace(req.Reference)
	req.ID = strings.TrimSpace(req.ID)
	return req, true
}

func resolveDockerActionNode(parent context.Context, requested string) (host string, isLocal bool, err error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || strings.EqualFold(requested, "local") || strings.EqualFold(requested, "localhost") {
		return "127.0.0.1", true, nil
	}

	localIPs := localIPv4AddressSet()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	nodes, localIP, discoverErr := discoverClusterNodeIPs(ctx)
	cancel()

	localIP = strings.TrimSpace(localIP)
	if localIP != "" {
		localIPs[localIP] = struct{}{}
	}

	if requested == "127.0.0.1" || requested == localIP || isKnownLocalIP(localIPs, requested) {
		return requested, true, nil
	}

	if discoverErr == nil {
		nodes = uniqueNonEmptyStrings(nodes)
		if len(nodes) > 0 && !containsString(nodes, requested) {
			return "", false, fmt.Errorf("node not found in autodiscovery: %s", requested)
		}
	}

	return requested, false, nil
}

func resolveDockerDeleteTarget(req dockerImageActionRequest) string {
	if id := strings.TrimSpace(req.ID); id != "" {
		return id
	}
	return strings.TrimSpace(req.Reference)
}

func isPullableDockerReference(reference string) bool {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return false
	}
	if strings.Contains(reference, "<none>") {
		return false
	}
	return true
}

func dockerActionNodeLabel(host string, isLocal bool) string {
	host = strings.TrimSpace(host)
	if host == "" {
		if isLocal {
			return "127.0.0.1"
		}
		return "unknown"
	}
	return host
}

func dockerImagesForNode(parent context.Context, host string, isLocal bool, timeout time.Duration) ([]dockerImageInfo, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	output, err := runClusterNodeShell(ctx, host, isLocal, dockerImagesListScript)
	if err != nil {
		label := strings.TrimSpace(host)
		if isLocal || label == "" {
			label = "local"
		}
		return nil, fmt.Errorf("docker images failed on %s: %w", label, err)
	}

	images, parseErr := parseDockerImagesOutput(output)
	if parseErr != nil {
		label := strings.TrimSpace(host)
		if isLocal || label == "" {
			label = "local"
		}
		return nil, fmt.Errorf("docker images parse failed on %s: %w", label, parseErr)
	}
	return images, nil
}

func compactDockerNodeImages(in []dockerNodeImages) []dockerNodeImages {
	out := make([]dockerNodeImages, 0, len(in))
	for _, node := range in {
		if strings.TrimSpace(node.NodeIP) == "" {
			continue
		}
		out = append(out, node)
	}
	return out
}

func sortDockerNodeImages(nodes []dockerNodeImages) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].IsLocal != nodes[j].IsLocal {
			return nodes[i].IsLocal
		}
		return strings.TrimSpace(nodes[i].NodeIP) < strings.TrimSpace(nodes[j].NodeIP)
	})
}

func pickLocalDockerImages(nodes []dockerNodeImages, fallback []dockerImageInfo) []dockerImageInfo {
	for _, node := range nodes {
		if !node.IsLocal {
			continue
		}
		if strings.TrimSpace(node.Error) != "" {
			continue
		}
		if node.Images == nil {
			return []dockerImageInfo{}
		}
		return node.Images
	}
	return fallback
}

func parseDockerImagesOutput(raw string) ([]dockerImageInfo, error) {
	lines := strings.Split(raw, "\n")
	images := make([]dockerImageInfo, 0, len(lines))

	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var row dockerImageCLIEntry
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("invalid docker images row %d: %w", idx+1, err)
		}

		repository := strings.TrimSpace(row.Repository)
		tag := strings.TrimSpace(row.Tag)
		id := strings.TrimSpace(row.ID)
		digest := strings.TrimSpace(row.Digest)
		size := strings.TrimSpace(row.Size)
		createdSince := strings.TrimSpace(row.CreatedSince)

		if repository == "" {
			repository = "<none>"
		}
		if tag == "" {
			tag = "<none>"
		}
		reference := repository + ":" + tag
		if reference == "<none>:<none>" && id != "" {
			reference = id
		}

		images = append(images, dockerImageInfo{
			Reference:    reference,
			Repository:   repository,
			Tag:          tag,
			ID:           id,
			Digest:       digest,
			Size:         size,
			CreatedSince: createdSince,
		})
	}

	sort.Slice(images, func(i, j int) bool {
		if images[i].Reference == images[j].Reference {
			return images[i].ID < images[j].ID
		}
		return images[i].Reference < images[j].Reference
	})

	return images, nil
}
