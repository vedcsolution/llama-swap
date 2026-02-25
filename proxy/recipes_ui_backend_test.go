package proxy

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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
	want := "docker ps --filter 'ancestor=nvcr.io/nvidia/tensorrt-llm/release:1.4.0' --format \"{{.Names}}\" | head -n 1"
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
