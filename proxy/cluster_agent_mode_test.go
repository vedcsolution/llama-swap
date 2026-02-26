package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterAgentMode_DiscoverClusterNodeIPsFromInventory(t *testing.T) {
	inventoryPath := writeTestClusterInventory(t)
	t.Setenv(clusterExecModeEnv, clusterExecModeAgent)
	t.Setenv(clusterInventoryFileEnv, inventoryPath)

	nodes, localIP, err := discoverClusterNodeIPs(context.Background())
	if err != nil {
		t.Fatalf("discoverClusterNodeIPs() error = %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes) = %d, want 2", len(nodes))
	}
	if nodes[0] != "10.10.0.11" || nodes[1] != "10.10.0.12" {
		t.Fatalf("unexpected nodes: %#v", nodes)
	}
	if localIP != "10.10.0.11" {
		t.Fatalf("localIP = %q, want %q", localIP, "10.10.0.11")
	}
}

func TestResolveDockerActionNode_AgentModeUsesInventory(t *testing.T) {
	inventoryPath := writeTestClusterInventory(t)
	t.Setenv(clusterExecModeEnv, clusterExecModeAgent)
	t.Setenv(clusterInventoryFileEnv, inventoryPath)

	headHost, headLocal, err := resolveDockerActionNode(context.Background(), "")
	if err != nil {
		t.Fatalf("resolveDockerActionNode(head) error = %v", err)
	}
	if headHost != "10.10.0.11" || !headLocal {
		t.Fatalf("head resolve = (%q,%v), want (%q,true)", headHost, headLocal, "10.10.0.11")
	}

	workerHost, workerLocal, err := resolveDockerActionNode(context.Background(), "192.168.8.122")
	if err != nil {
		t.Fatalf("resolveDockerActionNode(worker) error = %v", err)
	}
	if workerHost != "10.10.0.12" || workerLocal {
		t.Fatalf("worker resolve = (%q,%v), want (%q,false)", workerHost, workerLocal, "10.10.0.12")
	}
}

func TestBuildAgentShellExecCommand_UsesAgentEndpoint(t *testing.T) {
	cmd := buildAgentShellExecCommand("10.10.0.11", "echo ok")
	if cmd == "" {
		t.Fatal("buildAgentShellExecCommand returned empty command")
	}
	if !strings.Contains(cmd, clusterAgentShellPath) {
		t.Fatalf("command missing shell path %q: %s", clusterAgentShellPath, cmd)
	}
	if !strings.Contains(cmd, clusterAgentTokenFileEnv) {
		t.Fatalf("command missing token file env %q: %s", clusterAgentTokenFileEnv, cmd)
	}
}

func writeTestClusterInventory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "cluster-inventory.yaml")
	body := "" +
		"version: 1\n" +
		"rdma:\n" +
		"  required: true\n" +
		"  eth_if: enp1s0f1np1\n" +
		"  ib_if: rocep1s0f1,roceP2p1s0f1\n" +
		"agent:\n" +
		"  default_port: 19090\n" +
		"nodes:\n" +
		"  - id: head\n" +
		"    head: true\n" +
		"    data_ip: 10.10.0.11\n" +
		"    control_ip: 192.168.8.121\n" +
		"    proxy_ip: 192.168.8.121\n" +
		"    ssh_user: csolutions_ai\n" +
		"  - id: worker1\n" +
		"    data_ip: 10.10.0.12\n" +
		"    control_ip: 192.168.8.122\n" +
		"    proxy_ip: 192.168.8.122\n" +
		"    ssh_user: csolutions_ai\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return path
}
