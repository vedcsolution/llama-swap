package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	clusterAutodiscoverPathEnv = "LLAMA_SWAP_CLUSTER_AUTODISCOVER_PATH"
	clusterKVPrefix            = "__KV__"
	clusterStatusCacheTTLEnv   = "LLAMA_SWAP_CLUSTER_STATUS_CACHE_TTL_SECONDS"
	clusterStatusReadTimeout   = 25 * time.Second
	clusterNodeMetricTimeout   = 4 * time.Second
	clusterStorageNodeTimeout  = 4 * time.Second
)

type clusterNodeStatus struct {
	IP            string                 `json:"ip"`
	IsLocal       bool                   `json:"isLocal"`
	Port22Open    bool                   `json:"port22Open"`
	Port22Latency int64                  `json:"port22LatencyMs,omitempty"`
	SSHOK         bool                   `json:"sshOk"`
	SSHLatency    int64                  `json:"sshLatencyMs,omitempty"`
	Error         string                 `json:"error,omitempty"`
	DGX           *clusterDGXStatus      `json:"dgx,omitempty"`
	CPU           *clusterNodeCPUStatus  `json:"cpu,omitempty"`
	Disk          *clusterNodeDiskStatus `json:"disk,omitempty"`
	GPU           *clusterNodeGPUStatus  `json:"gpu,omitempty"`
}

type clusterNodeGPUDevice struct {
	Index          int  `json:"index"`
	UtilizationPct *int `json:"utilizationPct,omitempty"`
	TotalMiB       int  `json:"totalMiB"`
	UsedMiB        int  `json:"usedMiB"`
	FreeMiB        int  `json:"freeMiB"`
}

type clusterNodeCPUStatus struct {
	QueriedAt    string `json:"queriedAt"`
	UsagePercent *int   `json:"usagePercent,omitempty"`
	Error        string `json:"error,omitempty"`
}

type clusterNodeDiskStatus struct {
	QueriedAt    string `json:"queriedAt"`
	Mount        string `json:"mount,omitempty"`
	TotalMiB     int    `json:"totalMiB,omitempty"`
	UsedMiB      int    `json:"usedMiB,omitempty"`
	FreeMiB      int    `json:"freeMiB,omitempty"`
	UsagePercent *int   `json:"usagePercent,omitempty"`
	Error        string `json:"error,omitempty"`
}

type clusterNodeGPUStatus struct {
	QueriedAt string                 `json:"queriedAt"`
	Devices   []clusterNodeGPUDevice `json:"devices,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

type clusterDGXStatus struct {
	Supported       bool   `json:"supported"`
	CheckedAt       string `json:"checkedAt"`
	UpdateAvailable *bool  `json:"updateAvailable,omitempty"`
	RebootRunning   *bool  `json:"rebootRunning,omitempty"`
	UpgradeProgress *int   `json:"upgradeProgress,omitempty"`
	UpgradeStatus   string `json:"upgradeStatus,omitempty"`
	CacheProgress   *int   `json:"cacheProgress,omitempty"`
	CacheStatus     string `json:"cacheStatus,omitempty"`
	Error           string `json:"error,omitempty"`
}

type clusterStatusState struct {
	AutodiscoverPath string               `json:"autodiscoverPath"`
	DetectedAt       string               `json:"detectedAt"`
	LocalIP          string               `json:"localIp"`
	CIDR             string               `json:"cidr"`
	EthIF            string               `json:"ethIf"`
	IbIF             string               `json:"ibIf"`
	NodeCount        int                  `json:"nodeCount"`
	RemoteCount      int                  `json:"remoteCount"`
	ReachableBySSH   int                  `json:"reachableBySsh"`
	Overall          string               `json:"overall"`
	Summary          string               `json:"summary"`
	Errors           []string             `json:"errors,omitempty"`
	Nodes            []clusterNodeStatus  `json:"nodes"`
	Storage          *clusterStorageState `json:"storage,omitempty"`
}

type clusterStoragePathPresence struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

type clusterStorageNodeState struct {
	IP           string                       `json:"ip"`
	IsLocal      bool                         `json:"isLocal"`
	PresentCount int                          `json:"presentCount"`
	Paths        []clusterStoragePathPresence `json:"paths"`
}

type clusterStorageState struct {
	Paths          []string                  `json:"paths"`
	Nodes          []clusterStorageNodeState `json:"nodes"`
	DuplicatePaths []string                  `json:"duplicatePaths,omitempty"`
	SharedAllPaths []string                  `json:"sharedAllPaths,omitempty"`
	Note           string                    `json:"note"`
}

func (pm *ProxyManager) apiGetClusterStatus(c *gin.Context) {
	forceRefresh := isTruthy(strings.TrimSpace(c.Query("force"))) || isTruthy(strings.TrimSpace(c.Query("refresh")))
	state, err := pm.readClusterStatusCached(c.Request.Context(), forceRefresh)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":            err.Error(),
			"autodiscoverPath": clusterAutodiscoverPath(),
		})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (pm *ProxyManager) readClusterStatusCached(parentCtx context.Context, forceRefresh bool) (clusterStatusState, error) {
	return pm.readClusterStatusCachedWithLoader(parentCtx, forceRefresh, pm.readClusterStatus)
}

func (pm *ProxyManager) readClusterStatusCachedWithLoader(
	parentCtx context.Context,
	forceRefresh bool,
	loader func(context.Context) (clusterStatusState, error),
) (clusterStatusState, error) {
	ttl := clusterStatusCacheTTL()
	if ttl <= 0 {
		return loader(parentCtx)
	}

	pm.clusterStatusCacheMu.Lock()
	defer pm.clusterStatusCacheMu.Unlock()

	if !forceRefresh && !pm.clusterStatusCacheExpiresAt.IsZero() && time.Now().Before(pm.clusterStatusCacheExpiresAt) {
		return pm.clusterStatusCacheState, nil
	}

	state, err := loader(parentCtx)
	if err != nil {
		return clusterStatusState{}, err
	}

	pm.clusterStatusCacheState = state
	pm.clusterStatusCacheExpiresAt = time.Now().Add(ttl)
	return state, nil
}

func clusterStatusCacheTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv(clusterStatusCacheTTLEnv))
	if raw == "" {
		return 60 * time.Second
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 60 * time.Second
	}
	if seconds == 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (pm *ProxyManager) readClusterStatus(parentCtx context.Context) (clusterStatusState, error) {
	autodiscoverPath := clusterAutodiscoverPath()
	if stat, err := os.Stat(autodiscoverPath); err != nil || stat.IsDir() {
		return clusterStatusState{}, fmt.Errorf(
			"autodiscover.sh not found: %s (set %s or place autodiscover.sh in repo root)",
			autodiscoverPath,
			clusterAutodiscoverPathEnv,
		)
	}

	ctx, cancel := context.WithTimeout(parentCtx, clusterStatusReadTimeout)
	defer cancel()

	values, detectErrors := runAutodiscoverSnapshot(ctx, autodiscoverPath)
	nodes := parseNodesArg(values["NODES_ARG"])
	localIP := strings.TrimSpace(values["LOCAL_IP"])
	if localIP != "" && !containsString(nodes, localIP) {
		nodes = append([]string{localIP}, nodes...)
	}
	if len(nodes) == 0 && localIP != "" {
		nodes = []string{localIP}
	}

	nodeStatuses := make([]clusterNodeStatus, len(nodes))
	var wg sync.WaitGroup
	for idx := range nodes {
		idx := idx
		node := nodes[idx]
		nodeStatuses[idx] = clusterNodeStatus{
			IP:      node,
			IsLocal: node == localIP,
		}

		if nodeStatuses[idx].IsLocal {
			nodeStatuses[idx].Port22Open = true
			nodeStatuses[idx].SSHOK = true
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			errParts := make([]string, 0, 2)

			p22ok, p22lat, p22err := probePort22(node, 2*time.Second)
			nodeStatuses[idx].Port22Open = p22ok
			nodeStatuses[idx].Port22Latency = p22lat
			if p22err != nil {
				errParts = append(errParts, "port22: "+p22err.Error())
			}

			sshOK, sshLat, sshErr := probeSSH(ctx, node, 8*time.Second)
			nodeStatuses[idx].SSHOK = sshOK
			nodeStatuses[idx].SSHLatency = sshLat
			if sshErr != nil {
				errParts = append(errParts, "ssh: "+sshErr.Error())
			}

			if len(errParts) > 0 {
				nodeStatuses[idx].Error = strings.Join(errParts, "; ")
			}
		}()
	}
	wg.Wait()
	populateClusterCPUStatus(ctx, nodeStatuses)
	populateClusterDiskStatus(ctx, nodeStatuses)
	populateClusterGPUStatus(ctx, nodeStatuses)
	storage := buildClusterStorageState(ctx, nodeStatuses)

	dgxCtx, dgxCancel := context.WithTimeout(ctx, dgxClusterReadTimeout)
	populateClusterDGXStatus(dgxCtx, nodeStatuses)
	dgxCancel()

	sort.Slice(nodeStatuses, func(i, j int) bool {
		if nodeStatuses[i].IsLocal != nodeStatuses[j].IsLocal {
			return nodeStatuses[i].IsLocal
		}
		return nodeStatuses[i].IP < nodeStatuses[j].IP
	})

	reachableBySSH := 0
	remoteCount := 0
	for _, n := range nodeStatuses {
		if !n.IsLocal {
			remoteCount++
		}
		if n.SSHOK {
			reachableBySSH++
		}
	}

	overall := "healthy"
	switch {
	case len(nodeStatuses) == 0:
		overall = "error"
	case remoteCount == 0:
		overall = "solo"
	}

	for _, n := range nodeStatuses {
		if !n.IsLocal && (!n.Port22Open || !n.SSHOK) {
			overall = "degraded"
			break
		}
	}
	if len(detectErrors) > 0 && overall == "healthy" {
		overall = "degraded"
	}

	summary := buildClusterSummary(overall, len(nodeStatuses), remoteCount, reachableBySSH, detectErrors)
	return clusterStatusState{
		AutodiscoverPath: autodiscoverPath,
		DetectedAt:       time.Now().UTC().Format(time.RFC3339),
		LocalIP:          localIP,
		CIDR:             strings.TrimSpace(values["CIDR"]),
		EthIF:            strings.TrimSpace(values["ETH_IF"]),
		IbIF:             strings.TrimSpace(values["IB_IF"]),
		NodeCount:        len(nodeStatuses),
		RemoteCount:      remoteCount,
		ReachableBySSH:   reachableBySSH,
		Overall:          overall,
		Summary:          summary,
		Errors:           detectErrors,
		Nodes:            nodeStatuses,
		Storage:          storage,
	}, nil
}

func populateClusterCPUStatus(parentCtx context.Context, nodes []clusterNodeStatus) {
	var wg sync.WaitGroup
	for idx := range nodes {
		idx := idx
		node := nodes[idx]
		status := &clusterNodeCPUStatus{
			QueriedAt: time.Now().UTC().Format(time.RFC3339),
		}
		nodes[idx].CPU = status
		if !node.IsLocal && !node.SSHOK {
			status.Error = "ssh not available"
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			usage, err := queryNodeCPUUsage(parentCtx, node.IP, node.IsLocal)
			if err != nil {
				status.Error = err.Error()
			} else {
				status.UsagePercent = clusterIntPtr(usage)
			}
		}()
	}
	wg.Wait()
}

func queryNodeCPUUsage(parentCtx context.Context, host string, isLocal bool) (int, error) {
	ctx, cancel := context.WithTimeout(parentCtx, clusterNodeMetricTimeout)
	defer cancel()

	output, err := runClusterNodeShell(ctx, host, isLocal, clusterCPUUsageScript())
	if err != nil {
		return 0, err
	}

	value := strings.TrimSpace(output)
	usage, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu usage output: %q", value)
	}
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return usage, nil
}

func clusterCPUUsageScript() string {
	return strings.Join([]string{
		"set +e",
		"read _ u1 n1 s1 i1 io1 irq1 sirq1 st1 _ < /proc/stat",
		"idle1=$((i1 + io1))",
		"total1=$((u1 + n1 + s1 + i1 + io1 + irq1 + sirq1 + st1))",
		"sleep 0.2",
		"read _ u2 n2 s2 i2 io2 irq2 sirq2 st2 _ < /proc/stat",
		"idle2=$((i2 + io2))",
		"total2=$((u2 + n2 + s2 + i2 + io2 + irq2 + sirq2 + st2))",
		"dt=$((total2-total1))",
		"didle=$((idle2-idle1))",
		"if [ \"$dt\" -le 0 ]; then echo 0; exit 0; fi",
		"busy=$((dt-didle))",
		"pct=$(((busy*100 + dt/2)/dt))",
		"if [ \"$pct\" -lt 0 ]; then pct=0; fi",
		"if [ \"$pct\" -gt 100 ]; then pct=100; fi",
		"echo \"$pct\"",
	}, "\n")
}

func populateClusterDiskStatus(parentCtx context.Context, nodes []clusterNodeStatus) {
	var wg sync.WaitGroup
	for idx := range nodes {
		idx := idx
		node := nodes[idx]
		status := &clusterNodeDiskStatus{
			QueriedAt: time.Now().UTC().Format(time.RFC3339),
			Mount:     "/",
		}
		nodes[idx].Disk = status
		if !node.IsLocal && !node.SSHOK {
			status.Error = "ssh not available"
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			totalMiB, usedMiB, freeMiB, usagePct, err := queryNodeDiskUsage(parentCtx, node.IP, node.IsLocal)
			if err != nil {
				status.Error = err.Error()
			} else {
				status.TotalMiB = totalMiB
				status.UsedMiB = usedMiB
				status.FreeMiB = freeMiB
				status.UsagePercent = usagePct
			}
		}()
	}
	wg.Wait()
}

func queryNodeDiskUsage(parentCtx context.Context, host string, isLocal bool) (int, int, int, *int, error) {
	ctx, cancel := context.WithTimeout(parentCtx, clusterNodeMetricTimeout)
	defer cancel()

	output, err := runClusterNodeShell(ctx, host, isLocal, "df -Pk / | awk 'NR==2 {printf \"%s,%s,%s,%s\\n\",$2,$3,$4,$5}'")
	if err != nil {
		return 0, 0, 0, nil, err
	}

	parts := strings.Split(strings.TrimSpace(output), ",")
	if len(parts) != 4 {
		return 0, 0, 0, nil, fmt.Errorf("unexpected disk usage output: %q", strings.TrimSpace(output))
	}

	totalKB, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("invalid disk total: %q", strings.TrimSpace(parts[0]))
	}
	usedKB, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("invalid disk used: %q", strings.TrimSpace(parts[1]))
	}
	freeKB, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("invalid disk free: %q", strings.TrimSpace(parts[2]))
	}

	usageRaw := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[3]), "%"))
	usagePct := (*int)(nil)
	if usageRaw != "" {
		if usage, err := strconv.Atoi(usageRaw); err == nil {
			if usage < 0 {
				usage = 0
			}
			if usage > 100 {
				usage = 100
			}
			usagePct = clusterIntPtr(usage)
		}
	}
	if usagePct == nil && totalKB > 0 {
		used := (usedKB * 100) / totalKB
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		usagePct = clusterIntPtr(used)
	}

	return totalKB / 1024, usedKB / 1024, freeKB / 1024, usagePct, nil
}

func populateClusterGPUStatus(parentCtx context.Context, nodes []clusterNodeStatus) {
	var wg sync.WaitGroup
	for idx := range nodes {
		idx := idx
		node := nodes[idx]
		status := &clusterNodeGPUStatus{
			QueriedAt: time.Now().UTC().Format(time.RFC3339),
		}
		nodes[idx].GPU = status
		if !node.IsLocal && !node.SSHOK {
			status.Error = "ssh not available"
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			devices, err := queryNodeGPUMemory(parentCtx, node.IP, node.IsLocal)
			if err != nil {
				status.Error = err.Error()
			} else {
				status.Devices = devices
			}
		}()
	}
	wg.Wait()
}

func queryNodeGPUMemory(parentCtx context.Context, host string, isLocal bool) ([]clusterNodeGPUDevice, error) {
	ctx, cancel := context.WithTimeout(parentCtx, clusterNodeMetricTimeout)
	defer cancel()

	output, err := runClusterNodeShell(ctx, host, isLocal, "nvidia-smi --query-gpu=utilization.gpu,memory.total,memory.used,memory.free --format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		fallbackDevices, fallbackErr := queryNodeGPUDevicesByList(parentCtx, host, isLocal)
		if fallbackErr != nil {
			return []clusterNodeGPUDevice{}, nil
		}
		return fallbackDevices, nil
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "no devices were found") {
		return []clusterNodeGPUDevice{}, nil
	}

	lines := strings.Split(trimmed, "\n")
	devices := make([]clusterNodeGPUDevice, 0, len(lines))
	for idx, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi output: %q", line)
		}
		utilRaw := strings.TrimSpace(parts[0])
		totalRaw := strings.TrimSpace(parts[1])
		usedRaw := strings.TrimSpace(parts[2])
		freeRaw := strings.TrimSpace(parts[3])
		util, err := parseOptionalGPUValue(utilRaw, 100)
		if err != nil {
			return nil, fmt.Errorf("invalid GPU utilization: %s", utilRaw)
		}
		total, err := parseOptionalGPUValue(totalRaw, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid GPU total memory: %s", totalRaw)
		}
		used, err := parseOptionalGPUValue(usedRaw, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid GPU used memory: %s", usedRaw)
		}
		free, err := parseOptionalGPUValue(freeRaw, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid GPU free memory: %s", freeRaw)
		}
		totalValue := 0
		if total != nil {
			totalValue = *total
		}
		usedValue := 0
		if used != nil {
			usedValue = *used
		}
		freeValue := totalValue - usedValue
		if free != nil {
			freeValue = *free
		}
		if freeValue < 0 {
			freeValue = 0
		}
		if usedValue < 0 {
			usedValue = 0
		}
		if totalValue > 0 && usedValue > totalValue {
			usedValue = totalValue
		}
		devices = append(devices, clusterNodeGPUDevice{
			Index:          idx,
			UtilizationPct: util,
			TotalMiB:       totalValue,
			UsedMiB:        usedValue,
			FreeMiB:        freeValue,
		})
	}
	if len(devices) == 0 {
		fallbackDevices, fallbackErr := queryNodeGPUDevicesByList(parentCtx, host, isLocal)
		if fallbackErr == nil {
			return fallbackDevices, nil
		}
	}
	return devices, nil
}

func queryNodeGPUDevicesByList(parentCtx context.Context, host string, isLocal bool) ([]clusterNodeGPUDevice, error) {
	ctx, cancel := context.WithTimeout(parentCtx, clusterNodeMetricTimeout)
	defer cancel()

	output, err := runClusterNodeShell(ctx, host, isLocal, "nvidia-smi -L")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	devices := make([]clusterNodeGPUDevice, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "GPU ") {
			continue
		}
		devices = append(devices, clusterNodeGPUDevice{
			Index: len(devices),
		})
	}
	return devices, nil
}

func parseOptionalGPUValue(raw string, maxValue int) (*int, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "N/A") {
		return nil, nil
	}
	numeric, err := strconv.Atoi(value)
	if err != nil {
		return nil, err
	}
	if numeric < 0 {
		numeric = 0
	}
	if maxValue > 0 && numeric > maxValue {
		numeric = maxValue
	}
	return clusterIntPtr(numeric), nil
}

func clusterIntPtr(value int) *int {
	v := value
	return &v
}

func clusterAutodiscoverPath() string {
	if v := strings.TrimSpace(os.Getenv(clusterAutodiscoverPathEnv)); v != "" {
		return v
	}

	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "autodiscover.sh")
		if clusterFileExists(candidate) {
			return candidate
		}
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(exeDir, "autodiscover.sh"),
			filepath.Join(exeDir, "..", "autodiscover.sh"),
			filepath.Join(exeDir, "..", "..", "autodiscover.sh"),
		} {
			if clusterFileExists(candidate) {
				return candidate
			}
		}
	}

	if home := userHomeDir(); home != "" {
		candidate := filepath.Join(home, "swap-laboratories", "autodiscover.sh")
		if clusterFileExists(candidate) {
			return candidate
		}
	}

	return "autodiscover.sh"
}

func clusterFileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func runAutodiscoverSnapshot(ctx context.Context, autodiscoverPath string) (map[string]string, []string) {
	script := strings.Join([]string{
		"set +e",
		fmt.Sprintf("source %s", shellQuote(autodiscoverPath)),
		fmt.Sprintf("kv(){ printf '%s%%s=%%s\\n' \"$1\" \"$2\"; }", clusterKVPrefix),
		"detect_interfaces; _RC_IF=$?",
		"detect_local_ip; _RC_LOCAL=$?",
		"detect_nodes; _RC_NODES=$?",
		"kv DETECT_INTERFACES_RC \"${_RC_IF}\"",
		"kv DETECT_LOCAL_IP_RC \"${_RC_LOCAL}\"",
		"kv DETECT_NODES_RC \"${_RC_NODES}\"",
		"kv LOCAL_IP \"${LOCAL_IP:-}\"",
		"kv ETH_IF \"${ETH_IF:-}\"",
		"kv IB_IF \"${IB_IF:-}\"",
		"kv CIDR \"${CIDR:-}\"",
		"kv NODES_ARG \"${NODES_ARG:-}\"",
	}, "\n")

	cmd := exec.CommandContext(ctx, "bash", "-lc", script)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()

	values := make(map[string]string, 16)
	detectErrors := make([]string, 0, 4)
	for _, line := range strings.Split(output.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, clusterKVPrefix) {
			continue
		}

		kv := strings.TrimPrefix(line, clusterKVPrefix)
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		values[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}

	if runErr != nil && !errorsIsContextCanceled(runErr) {
		detectErrors = append(detectErrors, "autodiscover command failed: "+runErr.Error())
	}
	appendDetectRCError(&detectErrors, "detect_interfaces", values["DETECT_INTERFACES_RC"])
	appendDetectRCError(&detectErrors, "detect_local_ip", values["DETECT_LOCAL_IP_RC"])
	appendDetectRCError(&detectErrors, "detect_nodes", values["DETECT_NODES_RC"])

	return values, detectErrors
}

func appendDetectRCError(errors *[]string, stepName, rcRaw string) {
	if strings.TrimSpace(rcRaw) == "" {
		return
	}
	rc, err := strconv.Atoi(strings.TrimSpace(rcRaw))
	if err != nil {
		return
	}
	if rc != 0 {
		*errors = append(*errors, fmt.Sprintf("%s failed (exit %d)", stepName, rc))
	}
}

func parseNodesArg(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		node := strings.TrimSpace(p)
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

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func probePort22(host string, timeout time.Duration) (ok bool, latencyMs int64, err error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), timeout)
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return false, latencyMs, err
	}
	_ = conn.Close()
	return true, latencyMs, nil
}

func probeSSH(parent context.Context, host string, timeout time.Duration) (ok bool, latencyMs int64, err error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(
		ctx,
		"ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ConnectionAttempts=2",
		"-o", "StrictHostKeyChecking=accept-new",
		host,
		"true",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	latencyMs = time.Since(start).Milliseconds()
	if runErr != nil {
		msg := strings.TrimSpace(output.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) || errorsIsContextCanceled(runErr) {
			if msg == "" {
				msg = runErr.Error()
			}
			return false, latencyMs, fmt.Errorf("ssh timeout/canceled: %s", msg)
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return false, latencyMs, fmt.Errorf("%s", msg)
	}
	return true, latencyMs, nil
}

func buildClusterSummary(overall string, nodeCount, remoteCount, reachableBySSH int, detectErrors []string) string {
	switch overall {
	case "solo":
		return fmt.Sprintf("Modo solo: %d nodo local detectado.", nodeCount)
	case "healthy":
		return fmt.Sprintf("Cluster OK: %d/%d nodos con SSH operativo.", reachableBySSH, nodeCount)
	case "degraded":
		if len(detectErrors) > 0 {
			return fmt.Sprintf("Cluster degradado: %d aviso(s) de autodetección y %d/%d nodos con SSH operativo.", len(detectErrors), reachableBySSH, nodeCount)
		}
		return fmt.Sprintf("Cluster degradado: %d nodo(s) remoto(s), SSH operativo en %d/%d nodos.", remoteCount, reachableBySSH, nodeCount)
	default:
		return "No se pudo determinar el estado del cluster."
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func errorsIsContextCanceled(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "context canceled")
}

func buildClusterStorageState(parentCtx context.Context, nodes []clusterNodeStatus) *clusterStorageState {
	paths := clusterStorageCandidatePaths()
	if len(paths) == 0 || len(nodes) == 0 {
		return nil
	}

	storageNodes := make([]clusterStorageNodeState, len(nodes))
	var wg sync.WaitGroup
	for idx := range nodes {
		idx := idx
		node := nodes[idx]

		wg.Add(1)
		go func() {
			defer wg.Done()

			entry := clusterStorageNodeState{
				IP:      node.IP,
				IsLocal: node.IsLocal,
				Paths:   make([]clusterStoragePathPresence, 0, len(paths)),
			}

			if !node.IsLocal && !node.SSHOK {
				for _, path := range paths {
					entry.Paths = append(entry.Paths, clusterStoragePathPresence{
						Path:  path,
						Error: "ssh unavailable",
					})
				}
				storageNodes[idx] = entry
				return
			}

			ctx, cancel := context.WithTimeout(parentCtx, clusterStorageNodeTimeout)
			defer cancel()

			output, err := runClusterNodeShell(ctx, node.IP, node.IsLocal, clusterStoragePresenceScript(paths))
			if err != nil {
				for _, path := range paths {
					entry.Paths = append(entry.Paths, clusterStoragePathPresence{
						Path:  path,
						Error: err.Error(),
					})
				}
				storageNodes[idx] = entry
				return
			}

			seen := make(map[string]bool, len(paths))
			for _, line := range strings.Split(output, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "__SP__|") {
					continue
				}
				parts := strings.SplitN(strings.TrimPrefix(line, "__SP__|"), "|", 2)
				if len(parts) != 2 {
					continue
				}
				path := strings.TrimSpace(parts[0])
				exists := strings.TrimSpace(parts[1]) == "1"
				seen[path] = exists
			}

			for _, path := range paths {
				exists := seen[path]
				if exists {
					entry.PresentCount++
				}
				entry.Paths = append(entry.Paths, clusterStoragePathPresence{
					Path:   path,
					Exists: exists,
				})
			}
			storageNodes[idx] = entry
		}()
	}
	wg.Wait()

	presence := make(map[string]int, len(paths))
	reachableNodes := 0
	for _, node := range storageNodes {
		nodeReachable := false
		for _, p := range node.Paths {
			if p.Error == "" {
				nodeReachable = true
				if p.Exists {
					presence[p.Path]++
				}
			}
		}
		if nodeReachable {
			reachableNodes++
		}
	}

	duplicatePaths := make([]string, 0, len(paths))
	sharedAllPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if presence[path] >= 2 {
			duplicatePaths = append(duplicatePaths, path)
		}
		if reachableNodes > 1 && presence[path] == reachableNodes {
			sharedAllPaths = append(sharedAllPaths, path)
		}
	}

	return &clusterStorageState{
		Paths:          paths,
		Nodes:          storageNodes,
		DuplicatePaths: duplicatePaths,
		SharedAllPaths: sharedAllPaths,
		Note:           "Se mantienen las rutas actuales por nodo. Si una ruta aparece en varios nodos, existe duplicación local potencial; objetivo NVMe-oF: una sola ruta de lectura compartida.",
	}
}

func clusterStorageCandidatePaths() []string {
	paths := make([]string, 0, 10)

	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths,
			filepath.Join(home, ".cache", "huggingface", "hub"),
			filepath.Join(home, ".cache", "huggingface", "datasets"),
			filepath.Join(home, ".cache", "llama-benchy-intelligence"),
			filepath.Join(home, ".cache", "uv"),
		)
	}

	if hfHome := strings.TrimSpace(os.Getenv("HF_HOME")); hfHome != "" {
		paths = append(paths,
			filepath.Clean(hfHome),
			filepath.Join(hfHome, "hub"),
			filepath.Join(hfHome, "datasets"),
		)
	}

	if trCache := strings.TrimSpace(os.Getenv("TRANSFORMERS_CACHE")); trCache != "" {
		paths = append(paths, filepath.Clean(trCache))
	}

	return uniqueNonEmptyStrings(paths)
}

func clusterStoragePresenceScript(paths []string) string {
	lines := make([]string, 0, len(paths)+2)
	lines = append(lines, "set +e")
	for _, path := range paths {
		lines = append(lines,
			fmt.Sprintf(
				"if [ -e %s ]; then printf '__SP__|%%s|1\\n' %s; else printf '__SP__|%%s|0\\n' %s; fi",
				shellQuote(path),
				shellQuote(path),
				shellQuote(path),
			),
		)
	}
	return strings.Join(lines, "\n")
}
