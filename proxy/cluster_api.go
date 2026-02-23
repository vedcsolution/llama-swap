package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const clusterStopTimeoutEnv = "LLAMA_SWAP_CLUSTER_STOP_TIMEOUT_SECONDS"

type clusterStopResponse struct {
	Message string `json:"message"`
	Script  string `json:"script"`
	Output  string `json:"output,omitempty"`
}

func (pm *ProxyManager) apiStopCluster(c *gin.Context) {
	// Always unload currently managed llama-swap processes first.
	pm.StopProcesses(StopImmediately)

	scriptPath, scriptArgs := clusterStopScriptAndArgs()
	if _, err := os.Stat(scriptPath); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": "llama-swap processes unloaded; cluster stop script not found, skipped container stop",
			"script":  scriptPath,
		})
		return
	}

	timeout := clusterStopTimeout()
	baseCtx := context.WithoutCancel(c.Request.Context())
	ctx, cancel := context.WithTimeout(baseCtx, timeout)
	defer cancel()

	args := append([]string{scriptPath}, scriptArgs...)
	cmd := exec.CommandContext(ctx, "bash", args...)
	output, err := cmd.CombinedOutput()
	outputText := strings.TrimSpace(string(output))

	if ctx.Err() == context.DeadlineExceeded {
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error":  fmt.Sprintf("cluster stop timed out after %s", timeout.Truncate(time.Second)),
			"script": scriptPath,
			"output": outputText,
		})
		return
	}

	if err != nil {
		pm.proxyLogger.Errorf("cluster stop failed: %v output=%s", err, outputText)
		c.JSON(http.StatusBadGateway, gin.H{
			"error":  fmt.Sprintf("cluster stop failed: %v", err),
			"script": scriptPath,
			"output": outputText,
		})
		return
	}

	if outputText != "" {
		pm.proxyLogger.Infof("cluster stop output: %s", outputText)
	}

	c.JSON(http.StatusOK, clusterStopResponse{
		Message: "llama-swap processes unloaded and cluster containers stopped",
		Script:  scriptPath,
		Output:  outputText,
	})
}

func clusterStopScriptAndArgs() (string, []string) {
	backendDir := strings.TrimSpace(recipesBackendDir())
	if backendDir != "" {
		stopScript := filepath.Join(backendDir, "stop-cluster-containers.sh")
		if stat, err := os.Stat(stopScript); err == nil && !stat.IsDir() {
			return stopScript, []string{"--all-nodes"}
		}
		launchScript := filepath.Join(backendDir, "launch-cluster.sh")
		if stat, err := os.Stat(launchScript); err == nil && !stat.IsDir() {
			return launchScript, []string{"stop"}
		}
	}

	legacy := clusterLaunchScriptPath()
	return legacy, []string{"stop"}
}

func clusterStopTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(clusterStopTimeoutEnv)); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 8 * time.Minute
}

func clusterLaunchScriptPath() string {
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "launch-cluster.sh")
		if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
			return candidate
		}
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(exeDir, "launch-cluster.sh"),
			filepath.Join(exeDir, "..", "launch-cluster.sh"),
			filepath.Join(exeDir, "..", "..", "launch-cluster.sh"),
		} {
			if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
				return candidate
			}
		}
	}

	if home := userHomeDir(); home != "" {
		candidate := filepath.Join(home, "swap-laboratories", "launch-cluster.sh")
		if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
			return candidate
		}
	}

	return "launch-cluster.sh"
}
