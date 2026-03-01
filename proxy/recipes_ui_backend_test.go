package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/mostlygeek/llama-swap/proxy/config"
)

func TestDetectRecipeBackendKind(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/tmp/u/spark-vllm-docker", want: "vllm"},
		{path: "/tmp/u/sqlang-backend", want: "sqlang"},
		{path: "/tmp/u/trtllm-backend", want: "trtllm"},
		{path: "/tmp/u/spark-llama-cpp", want: "llamacpp"},
		{path: "/opt/custom-backend", want: "custom"},
	}

	for _, tc := range tests {
		got := detectRecipeBackendKind(tc.path)
		if got != tc.want {
			t.Fatalf("detectRecipeBackendKind(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestLatestTRTLLMTag(t *testing.T) {
	tags := []string{"1.3.0rc3", "1.2.5", "1.3.0", "1.3.0rc4", "1.4.0rc1", "latest", "1.4.0"}
	got := latestTRTLLMTag(tags)
	want := "1.4.0"
	if got != want {
		t.Fatalf("latestTRTLLMTag() = %q, want %q", got, want)
	}
}

func TestCompareTRTLLMTagVersion(t *testing.T) {
	a, ok := parseTRTLLMTagVersion("1.3.0rc3")
	if !ok {
		t.Fatalf("failed to parse a")
	}
	b, ok := parseTRTLLMTagVersion("1.3.0")
	if !ok {
		t.Fatalf("failed to parse b")
	}
	if compareTRTLLMTagVersion(a, b) >= 0 {
		t.Fatalf("expected rc version to be lower than stable")
	}

	c, ok := parseTRTLLMTagVersion("1.3.1")
	if !ok {
		t.Fatalf("failed to parse c")
	}
	if compareTRTLLMTagVersion(b, c) >= 0 {
		t.Fatalf("expected 1.3.0 < 1.3.1")
	}
}

func TestResolveTRTLLMSourceImagePrefersOverrideFile(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, trtllmSourceImageOverrideFile)
	overrideValue := "nvcr.io/nvidia/tensorrt-llm/release:1.4.0"
	if err := os.WriteFile(overridePath, []byte(overrideValue+"\n"), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	got := resolveTRTLLMSourceImage(dir, "")
	if got != overrideValue {
		t.Fatalf("resolveTRTLLMSourceImage() = %q, want %q", got, overrideValue)
	}
}

func TestResolveLLAMACPPSourceImagePrefersOverrideFile(t *testing.T) {
	dir := t.TempDir()
	overridePath := filepath.Join(dir, llamacppSourceImageOverrideFile)
	overrideValue := "llama-cpp-spark:custom"
	if err := os.WriteFile(overridePath, []byte(overrideValue+"\n"), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	got := resolveLLAMACPPSourceImage(dir, "")
	if got != overrideValue {
		t.Fatalf("resolveLLAMACPPSourceImage() = %q, want %q", got, overrideValue)
	}
}

func TestRecipeManagedModelInCatalog(t *testing.T) {
	catalog := map[string]RecipeCatalogItem{
		"qwen3-coder-next-vllm-next": {ID: "qwen3-coder-next-vllm-next"},
	}

	if !recipeManagedModelInCatalog(RecipeManagedModel{}, catalog) {
		t.Fatalf("empty recipeRef should be allowed")
	}
	if !recipeManagedModelInCatalog(RecipeManagedModel{RecipeRef: "qwen3-coder-next-vllm-next"}, catalog) {
		t.Fatalf("known recipeRef should be allowed")
	}
	if recipeManagedModelInCatalog(RecipeManagedModel{RecipeRef: "openai-gpt-oss-120b"}, catalog) {
		t.Fatalf("unknown recipeRef should be filtered out")
	}
}

func TestMarshalConfigRawMap_PrettyStylesLongCommands(t *testing.T) {
	longCmd := "bash -lc 'echo one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twentyone twentytwo twentythree twentyfour twentyfive twentysix twentyseven twentyeight twentynine thirty'"
	longStop := "bash -lc 'echo stop one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty'"

	root := map[string]any{
		"models": map[string]any{
			"test-model": map[string]any{
				"cmd":      longCmd,
				"cmdStop":  longStop,
				"proxy":    "http://127.0.0.1:9001",
				"ttl":      0,
				"aliases":  []any{},
				"metadata": map[string]any{},
			},
		},
	}

	rendered, err := marshalConfigRawMap(root)
	if err != nil {
		t.Fatalf("marshalConfigRawMap() error: %v", err)
	}

	text := string(rendered)
	if !strings.Contains(text, "cmd: >-") {
		t.Fatalf("expected folded style for cmd, got:\n%s", text)
	}
	if !strings.Contains(text, "cmdStop: >-") {
		t.Fatalf("expected folded style for cmdStop, got:\n%s", text)
	}

	conf, err := config.LoadConfigFromReader(bytes.NewReader(rendered))
	if err != nil {
		t.Fatalf("LoadConfigFromReader() error: %v\nrendered:\n%s", err, text)
	}
	got, _, ok := conf.FindConfig("test-model")
	if !ok {
		t.Fatalf("test-model not found after load")
	}
	if got.Cmd != longCmd {
		t.Fatalf("cmd changed after round-trip\nwant: %q\ngot:  %q", longCmd, got.Cmd)
	}
	if got.CmdStop != longStop {
		t.Fatalf("cmdStop changed after round-trip\nwant: %q\ngot:  %q", longStop, got.CmdStop)
	}
}

func TestMarshalConfigRawMap_DoesNotPrettyFormatEscapedQuoteCommands(t *testing.T) {
	remoteCmd := `bash -lc 'exec ssh -o BatchMode=yes -o StrictHostKeyChecking=no 192.0.2.10 "bash -lc \"VLLM_SPARK_EXTRA_DOCKER_ARGS=\\\"-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root -v /home/tester/.cache/nv/ComputeCache:/root/.nv/ComputeCache -e TORCHINDUCTOR_CACHE_DIR=/tmp/torchinductor_root -e CUDA_CACHE_PATH=/root/.nv/ComputeCache -e TRITON_CACHE_DIR=/root/.triton/cache\\\" exec /tmp/run-recipe.sh sample --solo --port 6001\""'`

	root := map[string]any{
		"models": map[string]any{
			"test-model-remote": map[string]any{
				"cmd":      remoteCmd,
				"proxy":    "http://127.0.0.1:9001",
				"ttl":      0,
				"aliases":  []any{},
				"metadata": map[string]any{},
			},
		},
	}

	rendered, err := marshalConfigRawMap(root)
	if err != nil {
		t.Fatalf("marshalConfigRawMap() error: %v", err)
	}

	text := string(rendered)
	if !strings.Contains(text, "cmd: >-") {
		t.Fatalf("expected folded style for escaped-quote cmd, got:\n%s", text)
	}

	conf, err := config.LoadConfigFromReader(bytes.NewReader(rendered))
	if err != nil {
		t.Fatalf("LoadConfigFromReader() error: %v\nrendered:\n%s", err, text)
	}
	got, _, ok := conf.FindConfig("test-model-remote")
	if !ok {
		t.Fatalf("test-model-remote not found after load")
	}
	if got.Cmd != remoteCmd {
		t.Fatalf("cmd changed after round-trip\nwant: %q\ngot:  %q", remoteCmd, got.Cmd)
	}
}

func TestRecipeEntryTargetsActiveBackend(t *testing.T) {
	active := filepath.Join(t.TempDir(), "backend-active")
	other := filepath.Join(t.TempDir(), "backend-other")

	if !recipeEntryTargetsActiveBackend(nil, active) {
		t.Fatalf("nil metadata should be allowed")
	}
	if !recipeEntryTargetsActiveBackend(map[string]any{}, active) {
		t.Fatalf("missing recipe metadata should be allowed")
	}

	metaActive := map[string]any{recipeMetadataKey: map[string]any{"backend_dir": active}}
	if !recipeEntryTargetsActiveBackend(metaActive, active) {
		t.Fatalf("matching backend_dir should be allowed")
	}

	metaOther := map[string]any{recipeMetadataKey: map[string]any{"backend_dir": other}}
	if recipeEntryTargetsActiveBackend(metaOther, active) {
		t.Fatalf("different backend_dir should be filtered out")
	}
}

func TestResolveHFDownloadScriptPathPrefersEnv(t *testing.T) {
	temp := t.TempDir()
	script := filepath.Join(temp, "hf-download.sh")
	t.Setenv(hfDownloadScriptPathEnv, script)

	if got := resolveHFDownloadScriptPath(); got != script {
		t.Fatalf("resolveHFDownloadScriptPath() = %q, want %q", got, script)
	}
}

func TestRecipeBackendActionsForKindIncludesHFDownload(t *testing.T) {
	temp := t.TempDir()
	script := filepath.Join(temp, "hf-download.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv(hfDownloadScriptPathEnv, script)

	actions := recipeBackendActionsForKind("vllm", temp, "")
	for _, action := range actions {
		if action.Action == "download_hf_model" {
			if !strings.Contains(action.CommandHint, script) {
				t.Fatalf("download_hf_model commandHint missing script path: %q", action.CommandHint)
			}
			return
		}
	}
	t.Fatalf("download_hf_model action not found")
}

func TestRecipeBackendActionsForKindDoesNotIncludeLLAMACPPSyncImage(t *testing.T) {
	temp := t.TempDir()
	hfScript := filepath.Join(temp, "hf-download.sh")
	if err := os.WriteFile(hfScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hf script: %v", err)
	}
	t.Setenv(hfDownloadScriptPathEnv, hfScript)

	actions := recipeBackendActionsForKind("llamacpp", temp, "")
	for _, action := range actions {
		if action.Action == "sync_llamacpp_image" {
			t.Fatalf("sync_llamacpp_image action should not be present")
		}
		if action.Action == "download_llamacpp_q8_model" || action.Action == "pull_llamacpp_image" || action.Action == "update_llamacpp_image" {
			t.Fatalf("unexpected legacy llama.cpp action present: %q", action.Action)
		}
	}
}

func TestNormalizeHFFormat(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "safetensors"},
		{in: "safetensor", want: "safetensors"},
		{in: "safetensors", want: "safetensors"},
		{in: "gguf", want: "gguf"},
		{in: "GGUF", want: "gguf"},
		{in: "unknown", want: ""},
	}

	for _, tc := range tests {
		got := normalizeHFFormat(tc.in)
		if got != tc.want {
			t.Fatalf("normalizeHFFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsRecipeBackendDir(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "spark-vllm-docker")
	if err := os.MkdirAll(filepath.Join(valid, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir valid recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(valid, "run-recipe.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write valid run-recipe.sh: %v", err)
	}

	invalidMissingScript := filepath.Join(root, "missing-script")
	if err := os.MkdirAll(filepath.Join(invalidMissingScript, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir missing-script recipes: %v", err)
	}

	invalidMissingRecipes := filepath.Join(root, "missing-recipes")
	if err := os.MkdirAll(invalidMissingRecipes, 0o755); err != nil {
		t.Fatalf("mkdir missing-recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(invalidMissingRecipes, "run-recipe.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write missing-recipes run-recipe.sh: %v", err)
	}

	if !isRecipeBackendDir(valid) {
		t.Fatalf("expected valid backend dir")
	}
	if isRecipeBackendDir(invalidMissingScript) {
		t.Fatalf("backend without run-recipe.sh should be invalid")
	}
	if isRecipeBackendDir(invalidMissingRecipes) {
		t.Fatalf("backend without recipes dir should be invalid")
	}
}

func TestDiscoverRecipeBackendsFromRoot(t *testing.T) {
	root := t.TempDir()
	makeBackend := func(name string, valid bool) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if valid {
			if err := os.MkdirAll(filepath.Join(dir, "recipes"), 0o755); err != nil {
				t.Fatalf("mkdir recipes %s: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "run-recipe.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatalf("write run-recipe.sh %s: %v", name, err)
			}
		}
		return dir
	}

	wantA := makeBackend("spark-llama-cpp", true)
	wantB := makeBackend("spark-vllm-docker", true)
	_ = makeBackend("broken-backend", false)

	got := discoverRecipeBackendsFromRoot(root)
	sort.Strings(got)
	want := []string{wantA, wantB}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("discoverRecipeBackendsFromRoot() len=%d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("discoverRecipeBackendsFromRoot()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveRecipeBackendDirFromMeta_UsesBackendField(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("models: {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("LLAMA_SWAP_CONFIG_PATH", cfgPath)

	backend := filepath.Join(root, "backend", "spark-vllm-docker")
	if err := os.MkdirAll(filepath.Join(backend, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir backend recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backend, "run-recipe.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write run-recipe.sh: %v", err)
	}

	got := resolveRecipeBackendDirFromMeta(recipeCatalogMeta{Backend: "spark-vllm-docker"}, "", "")
	want, err := filepath.Abs(backend)
	if err != nil {
		t.Fatalf("abs backend: %v", err)
	}
	if got != want {
		t.Fatalf("resolveRecipeBackendDirFromMeta() = %q, want %q", got, want)
	}
}

func TestResolveRecipeBackendDirFromMeta_DoesNotFallbackToActiveBackend(t *testing.T) {
	root := t.TempDir()
	activeBackend := filepath.Join(root, "spark-vllm-docker")
	if err := os.MkdirAll(filepath.Join(activeBackend, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir active backend recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeBackend, "run-recipe.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write active run-recipe.sh: %v", err)
	}
	t.Setenv(recipesBackendDirEnv, activeBackend)

	if got := resolveRecipeBackendDirFromMeta(recipeCatalogMeta{}, "", ""); got != "" {
		t.Fatalf("resolveRecipeBackendDirFromMeta() = %q, want empty", got)
	}
}

func TestUpsertRecipeModel_FailsWhenRecipeBackendIsUnresolved(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("models: {}\ngroups: {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	recipesDir := filepath.Join(root, "recipes")
	if err := os.MkdirAll(recipesDir, 0o755); err != nil {
		t.Fatalf("mkdir recipes: %v", err)
	}
	recipePath := filepath.Join(recipesDir, "missing-backend.yaml")
	recipeBody := "" +
		"name: Missing Backend\n" +
		"description: no backend key\n" +
		"model: test-model\n" +
		"runtime: vllm\n"
	if err := os.WriteFile(recipePath, []byte(recipeBody), 0o644); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	t.Setenv(recipesCatalogDirEnv, recipesDir)

	pm := &ProxyManager{configPath: configPath}
	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:   "m1",
		RecipeRef: "missing-backend",
	})
	if err == nil {
		t.Fatalf("expected upsertRecipeModel to fail when backend is unresolved")
	}
	if !strings.Contains(err.Error(), "backend not resolved") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildVLLMContainerDetectExpr_UsesShellLiteralFilter(t *testing.T) {
	got := buildVLLMContainerDetectExpr("nvcr.io/nvidia/tensorrt-llm/release:1.4.0")
	want := "docker ps --filter \"ancestor=nvcr.io/nvidia/tensorrt-llm/release:1.4.0\" --format \"{{.Names}}\" | head -n 1"
	if got != want {
		t.Fatalf("buildVLLMContainerDetectExpr() = %q, want %q", got, want)
	}
}

func TestQuoteForShellLiteral_EscapesSingleQuotes(t *testing.T) {
	in := "repo'evil;$(touch /tmp/pwn)"
	got := quoteForShellLiteral(in)
	want := "'repo'\"'\"'evil;$(touch /tmp/pwn)'"
	if got != want {
		t.Fatalf("quoteForShellLiteral() = %q, want %q", got, want)
	}
}

func TestBackendStopExprWithContainer_LlamaCppMatchesPortedNames(t *testing.T) {
	got := backendStopExprWithContainer("llamacpp", "")
	want := "LLAMA_CPP_CONTAINER=\"$(docker ps --format \"{{.Names}}\" | grep -E \"^llama_cpp_spark_.*_${PORT}$|^llama_cpp_spark_${PORT}$\" | head -n 1)\"; if [ -n \"$LLAMA_CPP_CONTAINER\" ]; then docker rm -f \"$LLAMA_CPP_CONTAINER\" >/dev/null 2>&1 || true; fi"
	if got != want {
		t.Fatalf("backendStopExprWithContainer() = %q, want %q", got, want)
	}
}

func TestProxyManager_detectHFModelFormat_UsesSnapshotAndReturnsGGUFHint(t *testing.T) {
	modelDir := t.TempDir()
	revision := "abc123"
	if err := os.MkdirAll(filepath.Join(modelDir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "refs", "main"), []byte(revision+"\n"), 0o644); err != nil {
		t.Fatalf("write refs/main: %v", err)
	}
	snapshotDir := filepath.Join(modelDir, "snapshots", revision, "weights")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "model-q4.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatalf("write gguf: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "readme.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatalf("write helper file: %v", err)
	}

	format, ggufHint, err := detectHFModelFormat(modelDir, "models--org--model-gguf", "org/model")
	if err != nil {
		t.Fatalf("detectHFModelFormat() error: %v", err)
	}
	if format != "gguf" {
		t.Fatalf("detectHFModelFormat() format = %q, want %q", format, "gguf")
	}
	if ggufHint != "weights/model-q4.gguf" {
		t.Fatalf("detectHFModelFormat() gguf hint = %q, want %q", ggufHint, "weights/model-q4.gguf")
	}
}

func TestProxyManager_detectHFModelFormat_DetectsSafetensors(t *testing.T) {
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("tensor"), 0o644); err != nil {
		t.Fatalf("write safetensors: %v", err)
	}

	format, ggufHint, err := detectHFModelFormat(modelDir, "models--org--model", "org/model")
	if err != nil {
		t.Fatalf("detectHFModelFormat() error: %v", err)
	}
	if format != "safetensors" {
		t.Fatalf("detectHFModelFormat() format = %q, want %q", format, "safetensors")
	}
	if ggufHint != "" {
		t.Fatalf("detectHFModelFormat() gguf hint = %q, want empty", ggufHint)
	}
}

func TestProxyManager_buildAutoGeneratedHFRecipeContent_GGUF(t *testing.T) {
	content, err := buildAutoGeneratedHFRecipeContent(
		"autogen/qwen-gguf-auto",
		"unsloth/Qwen3-32B-GGUF",
		"gguf",
		"spark-llama-cpp",
		"qwen3.gguf",
	)
	if err != nil {
		t.Fatalf("buildAutoGeneratedHFRecipeContent() error: %v", err)
	}
	body := string(content)
	if !strings.Contains(body, "runtime: llama-cpp") {
		t.Fatalf("generated gguf recipe missing llama-cpp runtime: %s", body)
	}
	if !strings.Contains(body, "backend: spark-llama-cpp") {
		t.Fatalf("generated gguf recipe missing backend name: %s", body)
	}
	if !strings.Contains(body, "gguf_file: qwen3.gguf") {
		t.Fatalf("generated gguf recipe missing gguf_file: %s", body)
	}
	if !strings.Contains(body, "llama-server") {
		t.Fatalf("generated gguf recipe missing llama-server command: %s", body)
	}
}

func TestProxyManager_buildAutoGeneratedHFRecipeContent_Safetensors(t *testing.T) {
	content, err := buildAutoGeneratedHFRecipeContent(
		"autogen/qwen-safetensors-auto",
		"Qwen/Qwen3-32B",
		"safetensors",
		"spark-vllm-docker",
		"",
	)
	if err != nil {
		t.Fatalf("buildAutoGeneratedHFRecipeContent() error: %v", err)
	}
	body := string(content)
	if !strings.Contains(body, "runtime: vllm") {
		t.Fatalf("generated safetensors recipe missing vllm runtime: %s", body)
	}
	if !strings.Contains(body, "backend: spark-vllm-docker") {
		t.Fatalf("generated safetensors recipe missing backend name: %s", body)
	}
	if !strings.Contains(body, "vllm serve {model}") {
		t.Fatalf("generated safetensors recipe missing vllm command: %s", body)
	}
	if strings.Contains(body, "gguf_file:") {
		t.Fatalf("generated safetensors recipe should not set gguf_file: %s", body)
	}
}

func TestProxyManager_ensureAutoGeneratedHFRecipeFile_CreatesAndReuses(t *testing.T) {
	backendDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(backendDir, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir recipes: %v", err)
	}

	recipeRef := "autogen/test-model"
	firstContent := []byte("name: first\n")
	path1, created1, err := ensureAutoGeneratedHFRecipeFile(backendDir, recipeRef, firstContent)
	if err != nil {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() first call error: %v", err)
	}
	if !created1 {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() first call created = false, want true")
	}
	if filepath.Base(path1) != "test-model.yaml" {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() file = %q, want base test-model.yaml", path1)
	}
	stored1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("read first recipe: %v", err)
	}
	if string(stored1) != string(firstContent) {
		t.Fatalf("first stored content mismatch: got %q want %q", string(stored1), string(firstContent))
	}

	secondContent := []byte("name: second\n")
	path2, created2, err := ensureAutoGeneratedHFRecipeFile(backendDir, recipeRef, secondContent)
	if err != nil {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() second call error: %v", err)
	}
	if created2 {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() second call created = true, want false")
	}
	if path2 != path1 {
		t.Fatalf("ensureAutoGeneratedHFRecipeFile() second path = %q, want %q", path2, path1)
	}
	stored2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("read second recipe: %v", err)
	}
	if string(stored2) != string(firstContent) {
		t.Fatalf("existing recipe should be preserved; got %q want %q", string(stored2), string(firstContent))
	}
}

func TestProxyManager_hfRecipeModelMatches_FindsManagedModel(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	cfgBody := "" +
		"models:\n" +
		"  models--unsloth--Qwen3.5-122B-A10B-GGUF:\n" +
		"    useModelName: unsloth/Qwen3.5-122B-A10B-GGUF\n" +
		"    metadata:\n" +
		"      recipe_ui:\n" +
		"        recipe_ref: autogen/models-unsloth-qwen3-5-122b-a10b-gguf-gguf-auto\n" +
		"groups: {}\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pm := &ProxyManager{configPath: cfgPath}
	matches := pm.hfRecipeModelMatches()
	if len(matches) == 0 {
		t.Fatalf("hfRecipeModelMatches() returned empty map")
	}

	byModel, ok := matches[normalizeHFModelKey("unsloth/Qwen3.5-122B-A10B-GGUF")]
	if !ok {
		t.Fatalf("hfRecipeModelMatches() missing useModelName match")
	}
	if byModel.RecipeRef == "" {
		t.Fatalf("useModelName match missing recipeRef")
	}
	if byModel.ModelEntryID != "models--unsloth--Qwen3.5-122B-A10B-GGUF" {
		t.Fatalf("useModelName match modelEntryID = %q", byModel.ModelEntryID)
	}

	byEntry, ok := matches[normalizeHFModelKey("models--unsloth--Qwen3.5-122B-A10B-GGUF")]
	if !ok {
		t.Fatalf("hfRecipeModelMatches() missing model entry id match")
	}
	if byEntry.RecipeRef != byModel.RecipeRef {
		t.Fatalf("entry match recipeRef = %q, want %q", byEntry.RecipeRef, byModel.RecipeRef)
	}
}

func TestProxyManager_listRecipeBackendHFModelsWithRecipeState_MarksExistingRecipe(t *testing.T) {
	setHFHubPathOverride("")
	t.Cleanup(func() { setHFHubPathOverride("") })

	root := t.TempDir()
	hubPath := filepath.Join(root, "hub")
	if err := os.MkdirAll(hubPath, 0o755); err != nil {
		t.Fatalf("mkdir hub: %v", err)
	}

	cacheDir := "models--unsloth--Qwen3.5-122B-A10B-GGUF"
	modelDir := filepath.Join(hubPath, cacheDir)
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatalf("mkdir model dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "weights.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatalf("write model file: %v", err)
	}
	t.Setenv(hfHubPathEnv, hubPath)

	cfgPath := filepath.Join(root, "config.yaml")
	cfgBody := "" +
		"models:\n" +
		"  models--unsloth--Qwen3.5-122B-A10B-GGUF:\n" +
		"    useModelName: unsloth/Qwen3.5-122B-A10B-GGUF\n" +
		"    metadata:\n" +
		"      recipe_ui:\n" +
		"        recipe_ref: autogen/models-unsloth-qwen3-5-122b-a10b-gguf-gguf-auto\n" +
		"groups: {}\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pm := &ProxyManager{configPath: cfgPath}
	state, err := pm.listRecipeBackendHFModelsWithRecipeState()
	if err != nil {
		t.Fatalf("listRecipeBackendHFModelsWithRecipeState() error: %v", err)
	}
	if len(state.Models) != 1 {
		t.Fatalf("listRecipeBackendHFModelsWithRecipeState() models len = %d, want 1", len(state.Models))
	}
	model := state.Models[0]
	if !model.HasRecipe {
		t.Fatalf("expected model.HasRecipe=true")
	}
	if model.ExistingRecipeRef == "" {
		t.Fatalf("expected ExistingRecipeRef to be set")
	}
	if model.ExistingModelEntryID != cacheDir {
		t.Fatalf("ExistingModelEntryID = %q, want %q", model.ExistingModelEntryID, cacheDir)
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_AddsLocalRuntimeCacheArgs(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: "bash -lc 'exec /tmp/run-recipe.sh sample --solo --port 6001'",
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	for _, item := range []string{
		"VLLM_SPARK_EXTRA_DOCKER_ARGS=",
		"-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root",
		"-v /home/tester/.cache/nv/ComputeCache:/root/.nv/ComputeCache",
		"-e TORCHINDUCTOR_CACHE_DIR=/tmp/torchinductor_root",
		"-e CUDA_CACHE_PATH=/root/.nv/ComputeCache",
		"-e TRITON_CACHE_DIR=/root/.triton/cache",
	} {
		if !strings.Contains(cmd, item) {
			t.Fatalf("normalized cmd missing %q: %s", item, cmd)
		}
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_Idempotent(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: "bash -lc 'exec /tmp/run-recipe.sh sample --solo --port 6001'",
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
					},
				},
			},
		},
	}

	first := normalizeLegacyVLLMConfigCommands(conf)
	second := normalizeLegacyVLLMConfigCommands(first)
	if second.Models["model-vllm"].Cmd != first.Models["model-vllm"].Cmd {
		t.Fatalf("expected idempotent normalization\nfirst:  %s\nsecond: %s", first.Models["model-vllm"].Cmd, second.Models["model-vllm"].Cmd)
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_MergesExistingExtraDockerArgs(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: `bash -lc 'VLLM_SPARK_EXTRA_DOCKER_ARGS="-e FOO=bar" exec /tmp/run-recipe.sh sample --solo --port 6001'`,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	if !strings.Contains(cmd, "-e FOO=bar") {
		t.Fatalf("expected existing extra docker arg to be preserved: %s", cmd)
	}
	if strings.Count(cmd, "-e FOO=bar") != 1 {
		t.Fatalf("expected existing extra docker arg once, got %d in: %s", strings.Count(cmd, "-e FOO=bar"), cmd)
	}
	if strings.Count(cmd, "-e TRITON_CACHE_DIR=/root/.triton/cache") != 1 {
		t.Fatalf("expected TRITON cache arg once, got %d in: %s", strings.Count(cmd, "-e TRITON_CACHE_DIR=/root/.triton/cache"), cmd)
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_LocalCommandHasValidShellQuoting(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: "bash -lc 'exec /tmp/run-recipe.sh sample --solo --port 6001'",
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmd): %v", err)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmd args: %#v", args)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd has invalid shell quoting: %v\ncmd=%s\nout=%s", err, cmd, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_MultilineCommandKeepsRealNewlines(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: "bash -lc 'if docker ps --format \"{{.Names}}\" | grep -q \"^vllm_node$\"\nthen\n  if ! docker exec vllm_node ray status >/dev/null 2>&1\n  then\n    (cd /tmp/backend && ./launch-cluster.sh -t vllm-node:latest stop >/dev/null 2>&1 || true)\n  fi\nfi\nexec /tmp/run-recipe.sh sample -n 192.0.2.10,192.0.2.11 --tp 2 --port 6001'",
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
						"mode":        "cluster",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmd): %v", err)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmd args: %#v", args)
	}
	if strings.Contains(args[2], `\n`) {
		t.Fatalf("expected real newlines in bash payload, found literal \\\\n: %q", args[2])
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd has invalid shell quoting: %v\ncmd=%s\nout=%s", err, cmd, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_RemoteCommandHasValidShellQuoting(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: `bash -lc 'exec ssh -o BatchMode=yes -o StrictHostKeyChecking=no 192.0.2.10 "bash -lc \"exec /tmp/run-recipe.sh sample --solo --port 6001\""'`,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
						"mode":        "solo",
						"nodes":       "192.0.2.10",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	if !strings.Contains(cmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS=") {
		t.Fatalf("expected runtime cache assignment in remote command, got: %s", cmd)
	}
	if strings.Contains(cmd, `VLLM_SPARK_EXTRA_DOCKER_ARGS='`) {
		t.Fatalf("unexpected single-quoted runtime cache assignment in remote command: %s", cmd)
	}

	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmd): %v", err)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmd args: %#v", args)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd has invalid shell quoting: %v\ncmd=%s\nout=%s", err, cmd, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_RemoteCommandWithEscapedAssignmentUnchanged(t *testing.T) {
	original := `bash -lc 'exec ssh -o BatchMode=yes -o StrictHostKeyChecking=no 192.0.2.10 "bash -lc \"VLLM_SPARK_EXTRA_DOCKER_ARGS=\\\"-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root -v /home/tester/.cache/nv/ComputeCache:/root/.nv/ComputeCache -e TORCHINDUCTOR_CACHE_DIR=/tmp/torchinductor_root -e CUDA_CACHE_PATH=/root/.nv/ComputeCache -e TRITON_CACHE_DIR=/root/.triton/cache\\\" exec /tmp/run-recipe.sh sample --solo --port 6001\""'`
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: original,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
						"mode":        "solo",
						"nodes":       "192.0.2.10",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd
	if cmd != original {
		t.Fatalf("remote escaped assignment should remain unchanged\nwant: %s\ngot:  %s", original, cmd)
	}
	if strings.Count(cmd, "-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root") != 1 {
		t.Fatalf("expected runtime cache mount once, got cmd: %s", cmd)
	}

	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmd): %v", err)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmd args: %#v", args)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd has invalid shell quoting: %v\ncmd=%s\nout=%s", err, cmd, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_RemoteCommandWithoutShellLiteralGetsRequoted(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				// Legacy shape: bash -lc without a quoted script payload.
				Cmd: `bash -lc exec ssh -o BatchMode=yes -o StrictHostKeyChecking=no 192.0.2.10 "bash -lc \"exec /tmp/run-recipe.sh sample --solo --port 6001\""`,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
						"mode":        "solo",
						"nodes":       "192.0.2.10",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmd): %v", err)
	}
	if len(args) != 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmd args: %#v", args)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("cmd has invalid shell quoting: %v\ncmd=%s\nout=%s", err, cmd, strings.TrimSpace(string(out)))
	}

	localArgs, err := config.SanitizeCommand(args[2])
	if err != nil {
		t.Fatalf("SanitizeCommand(local script): %v\nscript=%s", err, args[2])
	}
	if len(localArgs) < 2 {
		t.Fatalf("unexpected local script args: %#v", localArgs)
	}
	remoteArg := localArgs[len(localArgs)-1]
	if !strings.HasPrefix(remoteArg, "bash -lc ") {
		t.Fatalf("expected remote payload to be bash -lc, got: %s", remoteArg)
	}

	remoteArgs, err := config.SanitizeCommand(remoteArg)
	if err != nil {
		t.Fatalf("SanitizeCommand(remoteArg): %v\nremoteArg=%s", err, remoteArg)
	}
	if len(remoteArgs) != 3 || remoteArgs[0] != "bash" || remoteArgs[1] != "-lc" {
		t.Fatalf("unexpected remote args: %#v", remoteArgs)
	}
	remoteInner := remoteArgs[2]
	if strings.Contains(remoteInner, "VLLM_SPARK_EXTRA_DOCKER_ARGS=-v ") {
		t.Fatalf("runtime cache assignment is not quoted in remote inner command: %s", remoteInner)
	}
	if !strings.Contains(remoteInner, `VLLM_SPARK_EXTRA_DOCKER_ARGS="-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root`) {
		t.Fatalf("missing quoted runtime cache assignment in remote inner command: %s", remoteInner)
	}

	checkRemote := exec.Command("bash", "-n", "-c", remoteInner)
	if out, err := checkRemote.CombinedOutput(); err != nil {
		t.Fatalf("remote inner command has invalid shell quoting: %v\nremoteInner=%s\nout=%s", err, remoteInner, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_FixesLegacySingleQuotedAncestorFilter(t *testing.T) {
	conf := config.Config{
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				CmdStop: `bash -lc 'docker ps --filter 'ancestor=vllm-node:latest' --format "{{.Names}}"'`,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/opt/spark-vllm-docker",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmdStop := got.Models["model-vllm"].CmdStop
	if strings.Contains(cmdStop, "--filter 'ancestor=") {
		t.Fatalf("expected legacy single-quoted filter to be normalized: %s", cmdStop)
	}
	if !strings.Contains(cmdStop, `--filter "ancestor=vllm-node:latest"`) {
		t.Fatalf("expected normalized double-quoted filter: %s", cmdStop)
	}
}

func TestProxyManager_NormalizeLegacyVLLMConfigCommands_DoesNotRewriteClusterResetProbe(t *testing.T) {
	conf := config.Config{
		Macros: config.MacroList{
			{Name: "user_home", Value: "/home/tester"},
		},
		Models: map[string]config.ModelConfig{
			"model-vllm": {
				Cmd: `bash -lc 'if docker ps --format "{{.Names}}" | grep -q "^vllm_node$"; then if ! docker exec vllm_node ray status >/dev/null 2>&1; then (cd /tmp/backend && ./launch-cluster.sh -t vllm-node:latest stop >/dev/null 2>&1 || true); fi; fi; VLLM_SPARK_EXTRA_DOCKER_ARGS="-e FOO=bar" exec /tmp/run-recipe.sh sample -n 192.0.2.10,192.0.2.11 --tp 2 --port 6001'`,
				Metadata: map[string]any{
					recipeMetadataKey: map[string]any{
						"backend_dir": "/tmp/backend/spark-vllm-docker",
					},
				},
			},
		},
	}

	got := normalizeLegacyVLLMConfigCommands(conf)
	cmd := got.Models["model-vllm"].Cmd

	if strings.Contains(cmd, "No running vLLM container found") {
		t.Fatalf("cluster reset probe command must not be rewritten with VLLM guard: %s", cmd)
	}
	if strings.Contains(cmd, `VLLM_CONTAINER="$(`) {
		t.Fatalf("cluster reset probe command must not be rewritten with container detection: %s", cmd)
	}
	if !strings.Contains(cmd, "docker exec vllm_node ray status") {
		t.Fatalf("expected cluster reset probe to remain intact: %s", cmd)
	}
	if !strings.Contains(cmd, "-e FOO=bar") {
		t.Fatalf("expected existing runtime cache args to be preserved: %s", cmd)
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMIncludesRuntimeCacheArgs(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm", "vllm")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:   "runtime-vllm-model",
		RecipeRef: "runtime-vllm",
		Mode:      "solo",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	cmd, recipeMeta := readRecipeModelCommandAndMeta(t, cfgPath, "runtime-vllm-model")
	if !strings.Contains(cmd, `VLLM_SPARK_EXTRA_DOCKER_ARGS="${vllm_extra_docker_args}"`) {
		t.Fatalf("expected macro-based runtime cache assignment, got: %s", cmd)
	}
	if !strings.Contains(cmd, "${recipe_runner}") {
		t.Fatalf("expected recipe runner macro, got: %s", cmd)
	}

	loadedCmd, _ := readLoadedModelCommands(t, cfgPath, "runtime-vllm-model")
	for _, item := range []string{
		"VLLM_SPARK_EXTRA_DOCKER_ARGS=",
		"-v /home/tester/.cache/torchinductor:/tmp/torchinductor_root",
		"-v /home/tester/.cache/nv/ComputeCache:/root/.nv/ComputeCache",
		"-e TORCHINDUCTOR_CACHE_DIR=/tmp/torchinductor_root",
		"-e CUDA_CACHE_PATH=/root/.nv/ComputeCache",
		"-e TRITON_CACHE_DIR=/root/.triton/cache",
	} {
		if !strings.Contains(loadedCmd, item) {
			t.Fatalf("loaded cmd missing runtime cache item %q: %s", item, loadedCmd)
		}
	}

	if enabled, ok := recipeMeta["runtime_cache_policy_enabled"].(bool); !ok || !enabled {
		t.Fatalf("runtime_cache_policy_enabled missing or false: %#v", recipeMeta["runtime_cache_policy_enabled"])
	}
	if version := intFromAny(recipeMeta["runtime_cache_policy_version"]); version != 1 {
		t.Fatalf("runtime_cache_policy_version = %d, want 1", version)
	}
}

func TestProxyManager_UpsertRecipeModel_NonVLLMDoesNotInjectRuntimeCacheArgs(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-llama-cpp", "runtime-llama", "llama-cpp")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:   "runtime-llama-model",
		RecipeRef: "runtime-llama",
		Mode:      "solo",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	cmd, recipeMeta := readRecipeModelCommandAndMeta(t, cfgPath, "runtime-llama-model")
	if strings.Contains(cmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS=") {
		t.Fatalf("unexpected runtime cache policy in non-vLLM cmd: %s", cmd)
	}
	if _, ok := recipeMeta["runtime_cache_policy_enabled"]; ok {
		t.Fatalf("non-vLLM recipe should not include runtime cache policy metadata: %#v", recipeMeta)
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMSingleNodeInjectsRuntimeCacheArgsRemotely(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-node", "vllm")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-node-model",
		RecipeRef:      "runtime-vllm-node",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	cmd, _ := readRecipeModelCommandAndMeta(t, cfgPath, "runtime-vllm-node-model")
	if !strings.Contains(cmd, "exec ssh -o BatchMode=yes") {
		t.Fatalf("expected single-node ssh command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS=${vllm_extra_docker_args_escaped}") {
		t.Fatalf("expected macro-based remote runtime cache assignment, got: %s", cmd)
	}
	if strings.Contains(cmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS='") {
		t.Fatalf("remote runtime cache assignment must avoid single quotes: %s", cmd)
	}
	if strings.Contains(cmd, "bash -lc 'VLLM_SPARK_EXTRA_DOCKER_ARGS=") {
		t.Fatalf("runtime cache assignment should not be injected only in local shell: %s", cmd)
	}

	loadedCmd, _ := readLoadedModelCommands(t, cfgPath, "runtime-vllm-node-model")
	if !strings.Contains(loadedCmd, `VLLM_SPARK_EXTRA_DOCKER_ARGS=\\\"-v /home/tester/.cache/torchinductor`) {
		t.Fatalf("expected escaped runtime cache assignment in loaded remote command, got: %s", loadedCmd)
	}
}

func TestProxyManager_SyncManagedModelsWithRecipeDefaults_TP2PromotesCluster(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-sync", "vllm")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-sync-model",
		RecipeRef:      "runtime-vllm-sync",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	recipePath := filepath.Join(filepath.Dir(cfgPath), "recipes", "runtime-vllm-sync.yaml")
	recipeBody := "" +
		"name: Runtime Cache Recipe\n" +
		"description: test recipe\n" +
		"model: test/model\n" +
		"runtime: vllm\n" +
		"backend: spark-vllm-docker\n" +
		"defaults:\n" +
		"  tensor_parallel: 2\n"
	if err := os.WriteFile(recipePath, []byte(recipeBody), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}

	if err := pm.syncManagedModelsWithRecipeDefaults(context.Background(), "runtime-vllm-sync"); err != nil {
		t.Fatalf("syncManagedModelsWithRecipeDefaults() error: %v", err)
	}

	cmd, recipeMeta := readRecipeModelCommandAndMeta(t, cfgPath, "runtime-vllm-sync-model")
	if got := intFromAny(recipeMeta["tensor_parallel"]); got != 2 {
		t.Fatalf("tensor_parallel = %d, want 2 (meta=%#v)", got, recipeMeta)
	}
	if mode := strings.ToLower(strings.TrimSpace(getString(recipeMeta, "mode"))); mode != "cluster" {
		t.Fatalf("mode = %q, want cluster (meta=%#v)", mode, recipeMeta)
	}
	if strings.Contains(cmd, " --solo") {
		t.Fatalf("expected cluster command without --solo, got: %s", cmd)
	}
	if !strings.Contains(cmd, " --tp 2") {
		t.Fatalf("expected command to include --tp 2, got: %s", cmd)
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMClusterCmdUsesConditionalReset(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-cluster-reset", "vllm")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-cluster-reset-model",
		RecipeRef:      "runtime-vllm-cluster-reset",
		Mode:           "cluster",
		TensorParallel: 2,
		Nodes:          "192.0.2.10,192.0.2.11",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	cmd, _ := readRecipeModelCommandAndMeta(t, cfgPath, "runtime-vllm-cluster-reset-model")
	if !strings.Contains(cmd, "${vllm_cluster_reset_") {
		t.Fatalf("expected cluster reset macro usage in raw cmd, got: %s", cmd)
	}

	loadedCmd, _ := readLoadedModelCommands(t, cfgPath, "runtime-vllm-cluster-reset-model")
	if !strings.Contains(cmd, `if docker ps --format "{{.Names}}" | grep -q "^vllm_node$"`) {
		if !strings.Contains(loadedCmd, `if docker ps --format "{{.Names}}" | grep -q "^vllm_node$"`) {
			t.Fatalf("expected conditional cluster reset probe, got raw=%s loaded=%s", cmd, loadedCmd)
		}
	}
	if !regexp.MustCompile(`grep -q "\^vllm_node\$"\s*;?\s*then`).MatchString(loadedCmd) {
		t.Fatalf("expected conditional cluster reset probe, got: %s", loadedCmd)
	}
	if !strings.Contains(loadedCmd, "if ! docker exec vllm_node ray status >/dev/null 2>&1") {
		t.Fatalf("expected cluster health guard before stop, got: %s", loadedCmd)
	}
	if !regexp.MustCompile(`ray status >/dev/null 2>&1\s*;?\s*then`).MatchString(loadedCmd) {
		t.Fatalf("expected cluster health guard before stop, got: %s", loadedCmd)
	}
	if strings.Contains(loadedCmd, "bash -lc '(cd ") {
		t.Fatalf("unexpected unconditional reset prefix in cmd: %s", loadedCmd)
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMSingleNodeCmdStopHasValidShellQuoting(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-stop", "vllm")

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-stop-model",
		RecipeRef:      "runtime-vllm-stop",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	models := getMap(root, "models")
	modelMap, ok := models["runtime-vllm-stop-model"].(map[string]any)
	if !ok {
		t.Fatalf("model not found in config")
	}
	rawCmdStop := strings.TrimSpace(getString(modelMap, "cmdStop"))
	if !strings.Contains(rawCmdStop, "${vllm_cmdstop_remote_") {
		t.Fatalf("expected macro-based remote cmdStop, got: %s", rawCmdStop)
	}

	_, cmdStop := readLoadedModelCommands(t, cfgPath, "runtime-vllm-stop-model")
	if cmdStop == "" {
		t.Fatalf("cmdStop is empty")
	}

	args, err := config.SanitizeCommand(cmdStop)
	if err != nil {
		t.Fatalf("SanitizeCommand(cmdStop): %v", err)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("unexpected cmdStop args: %#v", args)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("cmdStop has invalid shell quoting: %v\ncmdStop=%s\nout=%s", err, cmdStop, strings.TrimSpace(string(out)))
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMLifecycleCreateEditDeleteKeepsConfigClean(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-lifecycle", "vllm")
	modelID := "runtime-vllm-lifecycle-model"

	_, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-vllm-lifecycle",
		Mode:           "cluster",
		TensorParallel: 2,
		Nodes:          "192.0.2.10,192.0.2.11",
		NonPrivileged:  true,
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel(create) error: %v", err)
	}

	rawCmd, rawMeta := readRecipeModelCommandAndMeta(t, cfgPath, modelID)
	if !strings.Contains(rawCmd, "${vllm_cluster_reset_") {
		t.Fatalf("expected cluster reset macro in create cmd, got: %s", rawCmd)
	}
	if !strings.Contains(rawCmd, `VLLM_SPARK_EXTRA_DOCKER_ARGS="${vllm_extra_docker_args}"`) {
		t.Fatalf("expected runtime cache macro in create cmd, got: %s", rawCmd)
	}
	if !strings.Contains(rawCmd, "--non-privileged") {
		t.Fatalf("expected --non-privileged in create cmd, got: %s", rawCmd)
	}
	if got := strings.TrimSpace(getString(rawMeta, "mode")); got != "cluster" {
		t.Fatalf("create mode = %q, want cluster", got)
	}
	if !getBool(rawMeta, "non_privileged") {
		t.Fatalf("create non_privileged = false, want true")
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap(create): %v", err)
	}
	rawModel := getMap(getMap(root, "models"), modelID)
	rawCmdStop := strings.TrimSpace(getString(rawModel, "cmdStop"))
	if !strings.Contains(rawCmdStop, "${vllm_cmdstop_") {
		t.Fatalf("expected macro-based create cmdStop, got: %s", rawCmdStop)
	}

	loadedCmd, loadedCmdStop := readLoadedModelCommands(t, cfgPath, modelID)
	if !strings.Contains(loadedCmd, "--tp 2") {
		t.Fatalf("expected --tp 2 in loaded create cmd, got: %s", loadedCmd)
	}
	if !strings.Contains(loadedCmd, "--non-privileged") {
		t.Fatalf("expected --non-privileged in loaded create cmd, got: %s", loadedCmd)
	}
	assertBashLCShellValid(t, loadedCmd, "create cmd")
	assertBashLCShellValid(t, loadedCmdStop, "create cmdStop")

	_, err = pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-vllm-lifecycle",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
		NonPrivileged:  false,
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel(update) error: %v", err)
	}

	rawCmd, rawMeta = readRecipeModelCommandAndMeta(t, cfgPath, modelID)
	if !strings.Contains(rawCmd, "exec ssh -o BatchMode=yes") {
		t.Fatalf("expected remote single-node cmd after update, got: %s", rawCmd)
	}
	if !strings.Contains(rawCmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS=${vllm_extra_docker_args_escaped}") {
		t.Fatalf("expected escaped runtime cache macro in update cmd, got: %s", rawCmd)
	}
	if strings.Contains(rawCmd, "--non-privileged") {
		t.Fatalf("expected update cmd without --non-privileged, got: %s", rawCmd)
	}
	if got := strings.TrimSpace(getString(rawMeta, "mode")); got != "solo" {
		t.Fatalf("update mode = %q, want solo", got)
	}
	if got := strings.TrimSpace(getString(rawMeta, "nodes")); got != "192.0.2.10" {
		t.Fatalf("update nodes = %q, want 192.0.2.10", got)
	}
	if getBool(rawMeta, "non_privileged") {
		t.Fatalf("update non_privileged = true, want false")
	}

	root, err = loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap(update): %v", err)
	}
	rawModel = getMap(getMap(root, "models"), modelID)
	rawCmdStop = strings.TrimSpace(getString(rawModel, "cmdStop"))
	if !strings.Contains(rawCmdStop, "${vllm_cmdstop_remote_") {
		t.Fatalf("expected macro-based remote cmdStop after update, got: %s", rawCmdStop)
	}

	loadedCmd, loadedCmdStop = readLoadedModelCommands(t, cfgPath, modelID)
	if !strings.Contains(loadedCmd, " --solo") {
		t.Fatalf("expected --solo in loaded update cmd, got: %s", loadedCmd)
	}
	if strings.Contains(loadedCmd, "--non-privileged") {
		t.Fatalf("expected loaded update cmd without --non-privileged, got: %s", loadedCmd)
	}
	assertBashLCShellValid(t, loadedCmd, "update cmd")
	assertBashLCShellValid(t, loadedCmdStop, "update cmdStop")

	if _, err := pm.deleteRecipeModel(modelID); err != nil {
		t.Fatalf("deleteRecipeModel() error: %v", err)
	}

	root, err = loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap(delete): %v", err)
	}
	if _, ok := getMap(root, "models")[modelID]; ok {
		t.Fatalf("model %s should be deleted from models", modelID)
	}
	for groupName, raw := range getMap(root, "groups") {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, member := range groupMembers(group) {
			if member == modelID {
				t.Fatalf("model %s still present in group %s after delete", modelID, groupName)
			}
		}
	}

	if _, err := config.LoadConfig(cfgPath); err != nil {
		t.Fatalf("config should remain valid after delete: %v", err)
	}
}

func TestRecipeModelAPI_LifecycleMaintainsDRYCommands(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-api", "vllm")
	modelID := "runtime-vllm-api-model"
	router := gin.New()
	router.POST("/api/recipes/models", pm.apiUpsertRecipeModel)
	router.DELETE("/api/recipes/models/:id", pm.apiDeleteRecipeModel)

	createBody, err := json.Marshal(upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-vllm-api",
		Mode:           "cluster",
		TensorParallel: 2,
		Nodes:          "192.0.2.10,192.0.2.11",
		NonPrivileged:  true,
	})
	if err != nil {
		t.Fatalf("json.Marshal(create): %v", err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/api/recipes/models", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	router.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create API status=%d body=%s", createRec.Code, createRec.Body.String())
	}

	rawCmd, rawMeta := readRecipeModelCommandAndMeta(t, cfgPath, modelID)
	if !strings.Contains(rawCmd, "${vllm_cluster_reset_") {
		t.Fatalf("expected create cmd to use cluster reset macro, got: %s", rawCmd)
	}
	if !strings.Contains(rawCmd, "--non-privileged") {
		t.Fatalf("expected create cmd to include --non-privileged, got: %s", rawCmd)
	}
	if !getBool(rawMeta, "non_privileged") {
		t.Fatalf("expected create metadata non_privileged=true")
	}

	updateBody, err := json.Marshal(upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-vllm-api",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
		NonPrivileged:  false,
	})
	if err != nil {
		t.Fatalf("json.Marshal(update): %v", err)
	}
	updateReq := httptest.NewRequest(http.MethodPost, "/api/recipes/models", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateRec := httptest.NewRecorder()
	router.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update API status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	rawCmd, rawMeta = readRecipeModelCommandAndMeta(t, cfgPath, modelID)
	if !strings.Contains(rawCmd, "exec ssh -o BatchMode=yes") {
		t.Fatalf("expected update cmd to switch to remote single-node launch, got: %s", rawCmd)
	}
	if !strings.Contains(rawCmd, "VLLM_SPARK_EXTRA_DOCKER_ARGS=${vllm_extra_docker_args_escaped}") {
		t.Fatalf("expected update cmd to use escaped runtime cache macro, got: %s", rawCmd)
	}
	if strings.Contains(rawCmd, "--non-privileged") {
		t.Fatalf("expected update cmd without --non-privileged, got: %s", rawCmd)
	}
	if mode := strings.TrimSpace(getString(rawMeta, "mode")); mode != "solo" {
		t.Fatalf("expected update metadata mode=solo, got %q", mode)
	}
	if nodes := strings.TrimSpace(getString(rawMeta, "nodes")); nodes != "192.0.2.10" {
		t.Fatalf("expected update metadata nodes=192.0.2.10, got %q", nodes)
	}

	loadedCmd, loadedCmdStop := readLoadedModelCommands(t, cfgPath, modelID)
	if !strings.Contains(loadedCmd, " --solo") {
		t.Fatalf("expected loaded update cmd to include --solo, got: %s", loadedCmd)
	}
	assertBashLCShellValid(t, loadedCmd, "api update cmd")
	assertBashLCShellValid(t, loadedCmdStop, "api update cmdStop")

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/recipes/models/"+modelID, nil)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete API status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap(delete): %v", err)
	}
	if _, ok := getMap(root, "models")[modelID]; ok {
		t.Fatalf("model %s should be deleted by API", modelID)
	}
}

func TestRecipeSourceAPI_SaveDoesNotRewriteConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-save-no-config", "vllm")
	modelID := "runtime-save-no-config-model"
	if _, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-save-no-config",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
	}); err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	beforeConfig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config before save: %v", err)
	}

	router := gin.New()
	router.PUT("/api/recipes/source", pm.apiSaveRecipeSource)
	updatedRecipe := "" +
		"name: Runtime Cache Recipe\n" +
		"description: updated recipe\n" +
		"model: test/model\n" +
		"runtime: vllm\n" +
		"backend: spark-vllm-docker\n" +
		"defaults:\n" +
		"  tensor_parallel: 2\n"
	reqBody, err := json.Marshal(recipeSourceUpdateRequest{
		RecipeRef: "runtime-save-no-config",
		Content:   updatedRecipe,
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/recipes/source", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save recipe API status=%d body=%s", rec.Code, rec.Body.String())
	}

	afterConfig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after save: %v", err)
	}
	if string(afterConfig) != string(beforeConfig) {
		t.Fatalf("config.yaml changed after recipe save\nbefore:\n%s\nafter:\n%s", string(beforeConfig), string(afterConfig))
	}
}

func TestRecipeModelAPI_DeleteCascadePurgesSharedRecipeModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-delete-cascade", "vllm")
	modelA := "runtime-delete-cascade-a"
	modelB := "runtime-delete-cascade-b"
	for _, modelID := range []string{modelA, modelB} {
		if _, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
			ModelID:        modelID,
			RecipeRef:      "runtime-delete-cascade",
			Mode:           "cluster",
			TensorParallel: 1,
			Nodes:          "192.0.2.10",
		}); err != nil {
			t.Fatalf("upsertRecipeModel(%s) error: %v", modelID, err)
		}
	}

	recipePath := filepath.Join(filepath.Dir(cfgPath), "recipes", "runtime-delete-cascade.yaml")
	if _, err := os.Stat(recipePath); err != nil {
		t.Fatalf("expected recipe file to exist before delete: %v", err)
	}

	router := gin.New()
	router.DELETE("/api/recipes/models/:id", pm.apiDeleteRecipeModel)
	req := httptest.NewRequest(http.MethodDelete, "/api/recipes/models/"+modelA+"?cascadeRecipe=true", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete cascade status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp recipeDeleteModelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if !resp.CascadeRecipe {
		t.Fatalf("expected cascadeRecipe=true response")
	}
	if len(resp.PurgedModelIDs) != 2 {
		t.Fatalf("expected 2 purged models, got %v", resp.PurgedModelIDs)
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	models := getMap(root, "models")
	if _, ok := models[modelA]; ok {
		t.Fatalf("model %s should be purged", modelA)
	}
	if _, ok := models[modelB]; ok {
		t.Fatalf("model %s should be purged", modelB)
	}
	if _, err := os.Stat(recipePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected recipe file to be deleted, stat err=%v", err)
	}
}

func TestRecipeModelAPI_DeleteLegacyDoesNotCascade(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-delete-legacy", "vllm")
	modelA := "runtime-delete-legacy-a"
	modelB := "runtime-delete-legacy-b"
	for _, modelID := range []string{modelA, modelB} {
		if _, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
			ModelID:        modelID,
			RecipeRef:      "runtime-delete-legacy",
			Mode:           "cluster",
			TensorParallel: 1,
			Nodes:          "192.0.2.10",
		}); err != nil {
			t.Fatalf("upsertRecipeModel(%s) error: %v", modelID, err)
		}
	}

	recipePath := filepath.Join(filepath.Dir(cfgPath), "recipes", "runtime-delete-legacy.yaml")
	router := gin.New()
	router.DELETE("/api/recipes/models/:id", pm.apiDeleteRecipeModel)
	req := httptest.NewRequest(http.MethodDelete, "/api/recipes/models/"+modelA, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete legacy status=%d body=%s", rec.Code, rec.Body.String())
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	models := getMap(root, "models")
	if _, ok := models[modelA]; ok {
		t.Fatalf("model %s should be removed", modelA)
	}
	if _, ok := models[modelB]; !ok {
		t.Fatalf("model %s should remain", modelB)
	}
	if _, err := os.Stat(recipePath); err != nil {
		t.Fatalf("recipe file should remain after legacy delete: %v", err)
	}
}

func TestRecipeSourceAPI_DeleteWithPurgeManaged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-delete-source", "vllm")
	for _, modelID := range []string{"runtime-delete-source-a", "runtime-delete-source-b"} {
		if _, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
			ModelID:        modelID,
			RecipeRef:      "runtime-delete-source",
			Mode:           "cluster",
			TensorParallel: 1,
			Nodes:          "192.0.2.10",
		}); err != nil {
			t.Fatalf("upsertRecipeModel(%s) error: %v", modelID, err)
		}
	}

	recipePath := filepath.Join(filepath.Dir(cfgPath), "recipes", "runtime-delete-source.yaml")
	router := gin.New()
	router.DELETE("/api/recipes/source", pm.apiDeleteRecipeSource)
	req := httptest.NewRequest(http.MethodDelete, "/api/recipes/source?recipeRef=runtime-delete-source&purgeManaged=true", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete source status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp recipeDeleteSourceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.PurgedModelIDs) != 2 {
		t.Fatalf("expected 2 purged models, got %v", resp.PurgedModelIDs)
	}

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	if got := len(getMap(root, "models")); got != 0 {
		t.Fatalf("expected no models after purge, got %d", got)
	}
	if _, err := os.Stat(recipePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected recipe file deleted, stat err=%v", err)
	}
}

func TestRecipeSourceAPI_SyncDefaultsManualEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-sync-defaults-api", "vllm")
	modelID := "runtime-sync-defaults-api-model"
	if _, err := pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        modelID,
		RecipeRef:      "runtime-sync-defaults-api",
		Mode:           "cluster",
		TensorParallel: 1,
		Nodes:          "192.0.2.10",
	}); err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	recipePath := filepath.Join(filepath.Dir(cfgPath), "recipes", "runtime-sync-defaults-api.yaml")
	recipeBody := "" +
		"name: Runtime Cache Recipe\n" +
		"description: test recipe\n" +
		"model: test/model\n" +
		"runtime: vllm\n" +
		"backend: spark-vllm-docker\n" +
		"defaults:\n" +
		"  tensor_parallel: 2\n"
	if err := os.WriteFile(recipePath, []byte(recipeBody), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}

	router := gin.New()
	router.POST("/api/recipes/source/sync-defaults", pm.apiSyncRecipeSourceDefaults)
	body, err := json.Marshal(recipeSourceSyncDefaultsRequest{RecipeRef: "runtime-sync-defaults-api"})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/recipes/source/sync-defaults", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync defaults status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp recipeSourceSyncDefaultsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.UpdatedModelIDs) != 1 || resp.UpdatedModelIDs[0] != modelID {
		t.Fatalf("unexpected updatedModelIds: %v", resp.UpdatedModelIDs)
	}

	_, recipeMeta := readRecipeModelCommandAndMeta(t, cfgPath, modelID)
	if got := intFromAny(recipeMeta["tensor_parallel"]); got != 2 {
		t.Fatalf("tensor_parallel = %d, want 2", got)
	}
}

func TestProxyManager_UpsertRecipeModel_VLLMClusterWithMultilineResetMacroRemainsShellValid(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-multiline-reset", "vllm")

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	macros := getMap(root, "macros")
	macros["vllm_cluster_reset_latest"] = strings.Join([]string{
		`if docker ps --format "{{.Names}}" | grep -q "^vllm_node$"`,
		"then",
		`  if ! docker exec vllm_node ray status >/dev/null 2>&1`,
		"  then",
		`    (cd /tmp/backend && ./launch-cluster.sh -t vllm-node:latest stop >/dev/null 2>&1 || true)`,
		"  fi",
		"fi",
	}, "\n")
	root["macros"] = macros
	if err := writeConfigRawMap(cfgPath, root); err != nil {
		t.Fatalf("writeConfigRawMap: %v", err)
	}

	_, err = pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-multiline-reset-model",
		RecipeRef:      "runtime-vllm-multiline-reset",
		Mode:           "cluster",
		TensorParallel: 2,
		Nodes:          "192.0.2.10,192.0.2.11",
	})
	if err != nil {
		t.Fatalf("upsertRecipeModel() error: %v", err)
	}

	loadedCmd, _ := readLoadedModelCommands(t, cfgPath, "runtime-vllm-multiline-reset-model")
	if strings.Contains(loadedCmd, "fi VLLM_SPARK_EXTRA_DOCKER_ARGS=") {
		t.Fatalf("cluster macro separator missing, got invalid command: %s", loadedCmd)
	}
	assertBashLCShellValid(t, loadedCmd, "cluster cmd with multiline reset macro")
}

func TestProxyManager_UpsertRecipeModel_RejectsInvalidBashMacroPayload(t *testing.T) {
	pm, cfgPath := newRuntimeCacheTestProxyManager(t, "spark-vllm-docker", "runtime-vllm-invalid-macro", "vllm")

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}
	macros := getMap(root, "macros")
	macros["vllm_cluster_reset_latest"] = strings.Join([]string{
		`if docker ps --format "{{.Names}}" | grep -q "^vllm_node$"`,
		"then",
		`  if ! docker exec vllm_node ray status >/dev/null 2>&1`,
		"  then",
		`    echo bad`,
		// Intentionally missing terminating "fi" to break syntax.
	}, "\n")
	root["macros"] = macros
	if err := writeConfigRawMap(cfgPath, root); err != nil {
		t.Fatalf("writeConfigRawMap: %v", err)
	}

	_, err = pm.upsertRecipeModel(context.Background(), upsertRecipeModelRequest{
		ModelID:        "runtime-vllm-invalid-macro-model",
		RecipeRef:      "runtime-vllm-invalid-macro",
		Mode:           "cluster",
		TensorParallel: 2,
		Nodes:          "192.0.2.10,192.0.2.11",
	})
	if err == nil {
		t.Fatalf("expected upsert to fail with invalid bash payload")
	}
	if !strings.Contains(err.Error(), "invalid bash -lc payload") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func newRuntimeCacheTestProxyManager(t *testing.T, backendName, recipeRef, runtime string) (*ProxyManager, string) {
	t.Helper()

	root := t.TempDir()
	runnerPath := filepath.Join(root, "run-recipe.sh")
	if err := os.WriteFile(runnerPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write root run-recipe.sh: %v", err)
	}
	cfgPath := filepath.Join(root, "config.yaml")
	cfgBody := "" +
		"models: {}\n" +
		"groups: {}\n" +
		"macros:\n" +
		"  user_home: /home/tester\n" +
		"  recipe_runner: " + runnerPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	backendDir := filepath.Join(root, "backend", backendName)
	if err := os.MkdirAll(filepath.Join(backendDir, "recipes"), 0o755); err != nil {
		t.Fatalf("mkdir backend recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backendDir, "run-recipe.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write run-recipe.sh: %v", err)
	}

	recipesDir := filepath.Join(root, "recipes")
	if err := os.MkdirAll(recipesDir, 0o755); err != nil {
		t.Fatalf("mkdir recipes dir: %v", err)
	}
	recipeBody := "" +
		"name: Runtime Cache Recipe\n" +
		"description: test recipe\n" +
		"model: test/model\n" +
		"runtime: " + runtime + "\n" +
		"backend: " + backendName + "\n"
	if err := os.WriteFile(filepath.Join(recipesDir, recipeRef+".yaml"), []byte(recipeBody), 0o644); err != nil {
		t.Fatalf("write recipe file: %v", err)
	}

	t.Setenv("LLAMA_SWAP_CONFIG_PATH", cfgPath)
	t.Setenv(recipesCatalogDirEnv, recipesDir)

	pm := &ProxyManager{
		configPath:     cfgPath,
		processGroups:  map[string]*ProcessGroup{},
		proxyLogger:    NewLogMonitorWriter(io.Discard),
		upstreamLogger: NewLogMonitorWriter(io.Discard),
	}
	return pm, cfgPath
}

func readRecipeModelCommandAndMeta(t *testing.T, cfgPath, modelID string) (string, map[string]any) {
	t.Helper()

	root, err := loadConfigRawMap(cfgPath)
	if err != nil {
		t.Fatalf("loadConfigRawMap: %v", err)
	}

	models := getMap(root, "models")
	modelMap, ok := models[modelID].(map[string]any)
	if !ok {
		t.Fatalf("model %s not found in config", modelID)
	}

	cmd := strings.TrimSpace(getString(modelMap, "cmd"))
	meta := getMap(modelMap, "metadata")
	recipeMeta := getMap(meta, recipeMetadataKey)
	return cmd, recipeMeta
}

func readLoadedModelCommands(t *testing.T, cfgPath, modelID string) (string, string) {
	t.Helper()

	conf, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", cfgPath, err)
	}
	modelCfg, _, ok := conf.FindConfig(modelID)
	if !ok {
		t.Fatalf("model %s not found after LoadConfig", modelID)
	}
	return strings.TrimSpace(modelCfg.Cmd), strings.TrimSpace(modelCfg.CmdStop)
}

func assertBashLCShellValid(t *testing.T, cmd string, label string) {
	t.Helper()
	args, err := config.SanitizeCommand(cmd)
	if err != nil {
		t.Fatalf("%s SanitizeCommand: %v\ncmd=%s", label, err, cmd)
	}
	if len(args) < 3 || args[0] != "bash" || args[1] != "-lc" {
		t.Fatalf("%s unexpected command args: %#v\ncmd=%s", label, args, cmd)
	}

	check := exec.Command("bash", "-n", "-c", args[2])
	out, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("%s has invalid shell quoting: %v\ncmd=%s\nout=%s", label, err, cmd, strings.TrimSpace(string(out)))
	}
}
