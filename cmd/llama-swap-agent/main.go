package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAgentListen                = ":19090"
	agentListenEnv                    = "LLAMA_SWAP_AGENT_LISTEN"
	agentTokenEnv                     = "LLAMA_SWAP_AGENT_TOKEN"
	agentTokenFileEnv                 = "LLAMA_SWAP_AGENT_TOKEN_FILE"
	clusterDefaultRDMARequiredEnv     = "LLAMA_SWAP_CLUSTER_RDMA_REQUIRED"
	clusterDefaultRDMAEthIfEnv        = "LLAMA_SWAP_CLUSTER_RDMA_ETH_IF"
	clusterDefaultRDMAIbIfEnv         = "LLAMA_SWAP_CLUSTER_RDMA_IB_IF"
	defaultShellOutputLimitBytes  int = 256 * 1024
)

type shellRequest struct {
	Script         string `json:"script,omitempty"`
	ScriptBase64   string `json:"scriptBase64,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type shellResponse struct {
	OK       bool   `json:"ok"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

type rdmaPreflightResponse struct {
	Required bool     `json:"required"`
	EthIF    string   `json:"ethIf,omitempty"`
	IbIF     []string `json:"ibIf,omitempty"`
	EthUp    bool     `json:"ethUp"`
	IbUp     []string `json:"ibUp,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

type cappedBuffer struct {
	max   int
	buf   []byte
	total int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max <= 0 {
		return len(p), nil
	}
	c.total += len(p)
	if len(p) >= c.max {
		c.buf = append(c.buf[:0], p[len(p)-c.max:]...)
		return len(p), nil
	}
	newLen := len(c.buf) + len(p)
	if newLen > c.max {
		trim := newLen - c.max
		if trim >= len(c.buf) {
			c.buf = c.buf[:0]
		} else {
			c.buf = c.buf[trim:]
		}
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return strings.TrimSpace(string(c.buf))
}

func main() {
	listen := flag.String("listen", "", "listen address")
	flag.Parse()

	addr := strings.TrimSpace(*listen)
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv(agentListenEnv))
	}
	if addr == "" {
		addr = defaultAgentListen
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", handleHealth)
	mux.Handle("/v1/ops/shell", withAuth(handleShell))
	mux.Handle("/v1/rdma/preflight", withAuth(handleRDMAPreflight))

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	done := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer close(done)
		sig := <-sigChan
		log.Printf("signal received: %v", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("llama-swap-agent listening on http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("agent server failed: %v", err)
	}
	<-done
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"hostname": hostname,
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

func handleShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req shellRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}

	script, err := requestScript(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx := r.Context()
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	output, exitCode, runErr := runShellWithContext(ctx, script)
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, shellResponse{
			OK:       false,
			Output:   output,
			ExitCode: exitCode,
			Error:    runErr.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, shellResponse{
		OK:       true,
		Output:   output,
		ExitCode: 0,
	})
}

func handleRDMAPreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	required := isTruthy(strings.TrimSpace(os.Getenv(clusterDefaultRDMARequiredEnv)))
	ethIF := strings.TrimSpace(os.Getenv(clusterDefaultRDMAEthIfEnv))
	ibList := splitCSV(strings.TrimSpace(os.Getenv(clusterDefaultRDMAIbIfEnv)))

	resp := rdmaPreflightResponse{
		Required: required,
		EthIF:    ethIF,
		IbIF:     ibList,
	}
	errorsList := make([]string, 0, 4)

	if ethIF != "" {
		exists, up := interfaceStatus(ethIF)
		resp.EthUp = exists && up
		if !exists {
			errorsList = append(errorsList, fmt.Sprintf("eth interface not found: %s", ethIF))
		} else if required && !up {
			errorsList = append(errorsList, fmt.Sprintf("eth interface down: %s", ethIF))
		}
	}

	upIB := make([]string, 0, len(ibList))
	for _, ib := range ibList {
		exists, up := interfaceStatus(ib)
		if exists && up {
			upIB = append(upIB, ib)
			continue
		}
		if !exists {
			errorsList = append(errorsList, fmt.Sprintf("ib interface not found: %s", ib))
			continue
		}
		if required {
			errorsList = append(errorsList, fmt.Sprintf("ib interface down: %s", ib))
		}
	}
	resp.IbUp = upIB
	resp.Errors = errorsList

	status := http.StatusOK
	if required && len(errorsList) > 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

func withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := configuredToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if got == "" || got != token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func configuredToken() (string, error) {
	if direct := strings.TrimSpace(os.Getenv(agentTokenEnv)); direct != "" {
		return direct, nil
	}
	path := strings.TrimSpace(os.Getenv(agentTokenFileEnv))
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file failed: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file is empty: %s", path)
	}
	return token, nil
}

func requestScript(req shellRequest) (string, error) {
	script := strings.TrimSpace(req.Script)
	if script == "" {
		encoded := strings.TrimSpace(req.ScriptBase64)
		if encoded == "" {
			return "", errors.New("script is required")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("invalid scriptBase64: %w", err)
		}
		script = strings.TrimSpace(string(decoded))
	}
	if script == "" {
		return "", errors.New("script is empty")
	}
	return script, nil
}

func runShellWithContext(parent context.Context, script string) (string, int, error) {
	cmd := exec.Command("bash", "-lc", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	output := &cappedBuffer{max: defaultShellOutputLimitBytes}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		return "", 0, err
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
			terminateProcessGroup(cmd.Process)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return output.String(), -1, fmt.Errorf("timeout: %w", parent.Err())
	}
	if errors.Is(parent.Err(), context.Canceled) {
		return output.String(), -1, fmt.Errorf("canceled: %w", parent.Err())
	}
	if waitErr == nil {
		return output.String(), 0, nil
	}

	exitCode := 1
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return output.String(), exitCode, waitErr
}

func terminateProcessGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = syscall.Kill(-proc.Pid, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	_ = proc.Kill()
}

func interfaceStatus(name string) (exists bool, up bool) {
	iface, err := net.InterfaceByName(strings.TrimSpace(name))
	if err != nil {
		return false, false
	}
	return true, (iface.Flags & net.FlagUp) != 0
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func isTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func parseInt(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
