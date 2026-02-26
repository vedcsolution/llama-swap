package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	clusterExecModeEnv            = "LLAMA_SWAP_CLUSTER_EXEC_MODE"
	clusterExecModeLocal          = "local"
	clusterExecModeAgent          = "agent"
	clusterInventoryFileEnv       = "LLAMA_SWAP_CLUSTER_INVENTORY_FILE"
	clusterHeadNodeEnv            = "LLAMA_SWAP_CLUSTER_HEAD_NODE"
	clusterDefaultSSHUserEnv      = "LLAMA_SWAP_CLUSTER_DEFAULT_SSH_USER"
	clusterAgentTokenFileEnv      = "LLAMA_SWAP_AGENT_TOKEN_FILE"
	clusterAgentDefaultPortEnv    = "LLAMA_SWAP_AGENT_DEFAULT_PORT"
	clusterAgentDefaultPort       = 19090
	clusterAgentHealthPath        = "/v1/health"
	clusterAgentShellPath         = "/v1/ops/shell"
	clusterDefaultRDMARequiredEnv = "LLAMA_SWAP_CLUSTER_RDMA_REQUIRED"
	clusterDefaultRDMAEthIfEnv    = "LLAMA_SWAP_CLUSTER_RDMA_ETH_IF"
	clusterDefaultRDMAIbIfEnv     = "LLAMA_SWAP_CLUSTER_RDMA_IB_IF"
)

type clusterInventoryConfig struct {
	Version int                        `yaml:"version"`
	RDMA    clusterInventoryRDMAConfig `yaml:"rdma"`
	Agent   clusterInventoryAgent      `yaml:"agent"`
	Nodes   []clusterInventoryNode     `yaml:"nodes"`
}

type clusterInventoryRDMAConfig struct {
	Required bool   `yaml:"required"`
	EthIF    string `yaml:"eth_if"`
	IbIF     string `yaml:"ib_if"`
}

type clusterInventoryAgent struct {
	DefaultPort int `yaml:"default_port"`
}

type clusterInventoryNode struct {
	ID        string `yaml:"id"`
	Head      bool   `yaml:"head"`
	DataIP    string `yaml:"data_ip"`
	ControlIP string `yaml:"control_ip"`
	ProxyIP   string `yaml:"proxy_ip"`
	SSHUser   string `yaml:"ssh_user"`
}

type clusterNodeRoute struct {
	ID        string
	Head      bool
	DataIP    string
	ControlIP string
	ProxyIP   string
	SSHUser   string
}

type agentShellRequest struct {
	Script         string `json:"script,omitempty"`
	ScriptBase64   string `json:"scriptBase64,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type agentShellResponse struct {
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

func clusterExecMode() string {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(clusterExecModeEnv)))
	switch mode {
	case clusterExecModeAgent:
		return clusterExecModeAgent
	default:
		return clusterExecModeLocal
	}
}

func clusterExecModeIsAgent() bool {
	return clusterExecMode() == clusterExecModeAgent
}

func clusterAgentTokenFilePath() string {
	return strings.TrimSpace(os.Getenv(clusterAgentTokenFileEnv))
}

func clusterAgentToken() (string, error) {
	path := clusterAgentTokenFilePath()
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read agent token file failed: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("agent token file is empty: %s", path)
	}
	return token, nil
}

func clusterAgentPort(defaultFromInventory int) int {
	if raw := strings.TrimSpace(os.Getenv(clusterAgentDefaultPortEnv)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 65535 {
			return parsed
		}
	}
	if defaultFromInventory > 0 && defaultFromInventory <= 65535 {
		return defaultFromInventory
	}
	return clusterAgentDefaultPort
}

func clusterInventoryFilePath() string {
	if v := strings.TrimSpace(os.Getenv(clusterInventoryFileEnv)); v != "" {
		return v
	}

	candidates := make([]string, 0, 8)
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			fmt.Sprintf("%s/cluster-inventory.yaml", wd),
			fmt.Sprintf("%s/cluster.inventory.yaml", wd),
		)
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "cluster-inventory.yaml"),
			filepath.Join(exeDir, "..", "cluster-inventory.yaml"),
			filepath.Join(exeDir, "..", "..", "cluster-inventory.yaml"),
		)
	}
	if home := userHomeDir(); home != "" {
		candidates = append(candidates,
			filepath.Join(home, "swap-laboratories", "cluster-inventory.yaml"),
		)
	}

	for _, path := range candidates {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	return ""
}

func loadClusterInventory() (clusterInventoryConfig, error) {
	path := clusterInventoryFilePath()
	if path == "" {
		return clusterInventoryConfig{}, fmt.Errorf("%s is required when %s=%s", clusterInventoryFileEnv, clusterExecModeEnv, clusterExecModeAgent)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return clusterInventoryConfig{}, fmt.Errorf("read inventory failed: %w", err)
	}

	var cfg clusterInventoryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return clusterInventoryConfig{}, fmt.Errorf("invalid inventory yaml: %w", err)
	}

	if len(cfg.Nodes) == 0 {
		return clusterInventoryConfig{}, fmt.Errorf("inventory has no nodes")
	}

	if cfg.Version == 0 {
		cfg.Version = 1
	}

	if cfg.RDMA.EthIF == "" {
		cfg.RDMA.EthIF = strings.TrimSpace(os.Getenv(clusterDefaultRDMAEthIfEnv))
	}
	if cfg.RDMA.IbIF == "" {
		cfg.RDMA.IbIF = strings.TrimSpace(os.Getenv(clusterDefaultRDMAIbIfEnv))
	}
	if !cfg.RDMA.Required {
		cfg.RDMA.Required = isTruthy(strings.TrimSpace(os.Getenv(clusterDefaultRDMARequiredEnv)))
	}

	return cfg, nil
}

func clusterInventoryRoutes() ([]clusterNodeRoute, clusterInventoryRDMAConfig, int, error) {
	cfg, err := loadClusterInventory()
	if err != nil {
		return nil, clusterInventoryRDMAConfig{}, 0, err
	}

	defaultSSHUser := strings.TrimSpace(os.Getenv(clusterDefaultSSHUserEnv))
	routes := make([]clusterNodeRoute, 0, len(cfg.Nodes))
	seenData := make(map[string]struct{}, len(cfg.Nodes))
	seenIDs := make(map[string]struct{}, len(cfg.Nodes))

	for idx, node := range cfg.Nodes {
		dataIP := strings.TrimSpace(node.DataIP)
		controlIP := strings.TrimSpace(node.ControlIP)
		proxyIP := strings.TrimSpace(node.ProxyIP)
		id := strings.TrimSpace(node.ID)
		sshUser := strings.TrimSpace(node.SSHUser)

		if dataIP == "" || controlIP == "" {
			return nil, clusterInventoryRDMAConfig{}, 0, fmt.Errorf("inventory node[%d] requires data_ip and control_ip", idx)
		}
		if proxyIP == "" {
			proxyIP = controlIP
		}
		if id == "" {
			id = dataIP
		}
		if sshUser == "" {
			sshUser = defaultSSHUser
		}

		if _, ok := seenData[dataIP]; ok {
			return nil, clusterInventoryRDMAConfig{}, 0, fmt.Errorf("duplicate data_ip in inventory: %s", dataIP)
		}
		seenData[dataIP] = struct{}{}
		if _, ok := seenIDs[id]; ok {
			return nil, clusterInventoryRDMAConfig{}, 0, fmt.Errorf("duplicate id in inventory: %s", id)
		}
		seenIDs[id] = struct{}{}

		routes = append(routes, clusterNodeRoute{
			ID:        id,
			Head:      node.Head,
			DataIP:    dataIP,
			ControlIP: controlIP,
			ProxyIP:   proxyIP,
			SSHUser:   sshUser,
		})
	}

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Head != routes[j].Head {
			return routes[i].Head
		}
		return routes[i].DataIP < routes[j].DataIP
	})

	return routes, cfg.RDMA, cfg.Agent.DefaultPort, nil
}

func clusterFindRoute(target string) (clusterNodeRoute, bool, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return clusterNodeRoute{}, false, nil
	}

	routes, _, _, err := clusterInventoryRoutes()
	if err != nil {
		return clusterNodeRoute{}, false, err
	}

	for _, route := range routes {
		if target == route.ID || target == route.DataIP || target == route.ControlIP || target == route.ProxyIP {
			return route, true, nil
		}
	}
	return clusterNodeRoute{}, false, nil
}

func clusterHeadRoute(preferred string) (clusterNodeRoute, error) {
	routes, _, _, err := clusterInventoryRoutes()
	if err != nil {
		return clusterNodeRoute{}, err
	}

	preferred = strings.TrimSpace(preferred)
	if preferred == "" {
		preferred = strings.TrimSpace(os.Getenv(clusterHeadNodeEnv))
	}
	if preferred != "" {
		for _, route := range routes {
			if preferred == route.ID || preferred == route.DataIP || preferred == route.ControlIP || preferred == route.ProxyIP {
				return route, nil
			}
		}
		return clusterNodeRoute{}, fmt.Errorf("preferred head node not found in inventory: %s", preferred)
	}

	for _, route := range routes {
		if route.Head {
			return route, nil
		}
	}
	if len(routes) == 0 {
		return clusterNodeRoute{}, fmt.Errorf("inventory has no nodes")
	}
	return routes[0], nil
}

func clusterInventoryContainsTarget(target string) bool {
	_, ok, err := clusterFindRoute(target)
	return err == nil && ok
}

func clusterAgentURL(route clusterNodeRoute, defaultPort int, path string) string {
	port := clusterAgentPort(defaultPort)
	host := strings.TrimSpace(route.ControlIP)
	return fmt.Sprintf("http://%s:%d%s", host, port, path)
}

func runClusterNodeAgentShell(ctx context.Context, route clusterNodeRoute, defaultPort int, script string) (string, error) {
	timeoutSeconds := 0
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			timeoutSeconds = int(remaining.Seconds())
		}
	}

	payload := agentShellRequest{
		Script:         script,
		TimeoutSeconds: timeoutSeconds,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal agent shell request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clusterAgentURL(route, defaultPort, clusterAgentShellPath), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build agent request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, tokenErr := clusterAgentToken(); tokenErr != nil {
		return "", tokenErr
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent shell request failed for %s: %w", route.ControlIP, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed agentShellResponse
	_ = json.Unmarshal(respBody, &parsed)

	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(parsed.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(respBody))
		}
		if msg == "" {
			msg = resp.Status
		}
		output := strings.TrimSpace(parsed.Output)
		if output != "" {
			msg += ": " + output
		}
		return "", fmt.Errorf("agent shell failed on %s: %s", route.ControlIP, msg)
	}
	return strings.TrimSpace(parsed.Output), nil
}

func probeAgent(parent context.Context, host string, timeout time.Duration) (ok bool, latencyMs int64, err error) {
	route, found, findErr := clusterFindRoute(host)
	if findErr != nil {
		return false, 0, findErr
	}
	if !found {
		return false, 0, fmt.Errorf("agent route not found for host: %s", host)
	}

	_, _, defaultPort, invErr := clusterInventoryRoutes()
	if invErr != nil {
		return false, 0, invErr
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clusterAgentURL(route, defaultPort, clusterAgentHealthPath), nil)
	if err != nil {
		return false, 0, err
	}
	if token, tokenErr := clusterAgentToken(); tokenErr != nil {
		return false, 0, tokenErr
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return false, latencyMs, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, latencyMs, fmt.Errorf("agent health status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, latencyMs, nil
}
