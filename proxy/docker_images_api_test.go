package proxy

import (
	"os"
	"testing"
)

func TestParseDockerImagesOutput(t *testing.T) {
	raw := `{"Repository":"llama-cpp-spark","Tag":"last","ID":"sha256:aaa","Digest":"<none>","CreatedSince":"2 hours ago","Size":"5.2GB"}
{"Repository":"nvidia/vllm","Tag":"26.01-py3","ID":"sha256:bbb","Digest":"sha256:ccc","CreatedSince":"5 days ago","Size":"8.7GB"}
`

	images, err := parseDockerImagesOutput(raw)
	if err != nil {
		t.Fatalf("parseDockerImagesOutput() error = %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("len(images) = %d, want 2", len(images))
	}
	if images[0].Reference != "llama-cpp-spark:last" {
		t.Fatalf("images[0].Reference = %q, want %q", images[0].Reference, "llama-cpp-spark:last")
	}
	if images[1].Reference != "nvidia/vllm:26.01-py3" {
		t.Fatalf("images[1].Reference = %q, want %q", images[1].Reference, "nvidia/vllm:26.01-py3")
	}
}

func TestParseDockerImagesOutput_InvalidJSON(t *testing.T) {
	_, err := parseDockerImagesOutput(`{"Repository":"ok","Tag":"v1"}
{bad-json}
`)
	if err == nil {
		t.Fatal("expected parse error for invalid json row")
	}
}

func TestSortDockerNodeImages_LocalFirst(t *testing.T) {
	nodes := []dockerNodeImages{
		{NodeIP: "192.168.8.123", IsLocal: false},
		{NodeIP: "192.168.8.121", IsLocal: true},
		{NodeIP: "192.168.8.122", IsLocal: false},
	}
	sortDockerNodeImages(nodes)

	if nodes[0].NodeIP != "192.168.8.121" || !nodes[0].IsLocal {
		t.Fatalf("expected local node first, got %#v", nodes[0])
	}
	if nodes[1].NodeIP != "192.168.8.122" || nodes[2].NodeIP != "192.168.8.123" {
		t.Fatalf("expected remote nodes sorted by ip, got %#v", nodes)
	}
}

func TestPickLocalDockerImages_PrefersLocalNode(t *testing.T) {
	fallback := []dockerImageInfo{{Reference: "fallback:latest"}}
	nodes := []dockerNodeImages{
		{NodeIP: "192.168.8.122", IsLocal: false, Images: []dockerImageInfo{{Reference: "remote:latest"}}},
		{NodeIP: "192.168.8.121", IsLocal: true, Images: []dockerImageInfo{{Reference: "local:latest"}}},
	}

	picked := pickLocalDockerImages(nodes, fallback)
	if len(picked) != 1 || picked[0].Reference != "local:latest" {
		t.Fatalf("expected local images, got %#v", picked)
	}
}

func TestPickLocalDockerImages_FallbackWhenLocalFails(t *testing.T) {
	fallback := []dockerImageInfo{{Reference: "fallback:latest"}}
	nodes := []dockerNodeImages{
		{NodeIP: "192.168.8.121", IsLocal: true, Error: "ssh failed"},
	}

	picked := pickLocalDockerImages(nodes, fallback)
	if len(picked) != 1 || picked[0].Reference != "fallback:latest" {
		t.Fatalf("expected fallback images, got %#v", picked)
	}
}

func TestPickPrimaryDockerImages_UsesRemoteWhenLocalUnavailable(t *testing.T) {
	fallback := []dockerImageInfo{{Reference: "fallback:latest"}}
	nodes := []dockerNodeImages{
		{NodeIP: "192.168.8.121", IsLocal: true, Error: "docker missing"},
		{NodeIP: "192.168.8.122", IsLocal: false, Images: []dockerImageInfo{{Reference: "remote:latest"}}},
	}

	picked := pickPrimaryDockerImages(nodes, fallback)
	if len(picked) != 1 || picked[0].Reference != "remote:latest" {
		t.Fatalf("expected remote images, got %#v", picked)
	}
}

func TestPickPrimaryDockerImages_ReturnsFallbackWhenNoReachableNodes(t *testing.T) {
	fallback := []dockerImageInfo{{Reference: "fallback:latest"}}
	nodes := []dockerNodeImages{
		{NodeIP: "192.168.8.121", IsLocal: true, Error: "docker missing"},
		{NodeIP: "192.168.8.122", IsLocal: false, Error: "ssh failed"},
	}

	picked := pickPrimaryDockerImages(nodes, fallback)
	if len(picked) != 1 || picked[0].Reference != "fallback:latest" {
		t.Fatalf("expected fallback images, got %#v", picked)
	}
}

func TestClusterExecMode_AutoSwitchesToAgentWhenInventoryExists(t *testing.T) {
	inventoryPath := writeTestClusterInventory(t)
	t.Setenv(clusterExecModeEnv, "")
	t.Setenv(clusterInventoryFileEnv, inventoryPath)
	if got := clusterExecMode(); got != clusterExecModeAgent {
		t.Fatalf("clusterExecMode() = %q, want %q", got, clusterExecModeAgent)
	}
}

func TestClusterExecMode_DefaultsToLocalWithoutInventory(t *testing.T) {
	temp := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(temp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
	})

	t.Setenv(clusterExecModeEnv, "")
	t.Setenv(clusterInventoryFileEnv, "")
	if got := clusterExecMode(); got != clusterExecModeLocal {
		t.Fatalf("clusterExecMode() = %q, want %q", got, clusterExecModeLocal)
	}
}

func TestResolveDockerDeleteTarget(t *testing.T) {
	if got := resolveDockerDeleteTarget(dockerImageActionRequest{ID: "sha256:abc", Reference: "repo:tag"}); got != "sha256:abc" {
		t.Fatalf("expected ID to win, got %q", got)
	}
	if got := resolveDockerDeleteTarget(dockerImageActionRequest{Reference: "repo:tag"}); got != "repo:tag" {
		t.Fatalf("expected reference fallback, got %q", got)
	}
	if got := resolveDockerDeleteTarget(dockerImageActionRequest{}); got != "" {
		t.Fatalf("expected empty target, got %q", got)
	}
}

func TestIsPullableDockerReference(t *testing.T) {
	cases := []struct {
		reference string
		want      bool
	}{
		{reference: "llama-cpp-spark:last", want: true},
		{reference: "nvidia/vllm:26.01-py3", want: true},
		{reference: "", want: false},
		{reference: "<none>:<none>", want: false},
		{reference: "<none>", want: false},
	}

	for _, tc := range cases {
		got := isPullableDockerReference(tc.reference)
		if got != tc.want {
			t.Fatalf("isPullableDockerReference(%q)=%v want %v", tc.reference, got, tc.want)
		}
	}
}
