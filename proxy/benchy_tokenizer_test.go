package proxy

import (
	"testing"

	"github.com/mostlygeek/llama-swap/proxy/config"
)

func TestBenchyNormalizeLlamaTokenizer(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strip dash gguf suffix",
			in:   "unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF",
			want: "unsloth/Qwen3-Next-80B-A3B-Thinking",
		},
		{
			name: "strip underscore gguf suffix",
			in:   "org/model_gguf",
			want: "org/model",
		},
		{
			name: "strip file extension",
			in:   "org/model.gguf",
			want: "org/model",
		},
		{
			name: "keep non gguf tokenizer",
			in:   "Qwen/Qwen3-Next-80B-A3B-Thinking",
			want: "Qwen/Qwen3-Next-80B-A3B-Thinking",
		},
		{
			name: "keep absolute path",
			in:   "/models/tokenizer",
			want: "/models/tokenizer",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := benchyNormalizeLlamaTokenizer(tc.in); got != tc.want {
				t.Fatalf("benchyNormalizeLlamaTokenizer(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestProxyManager_BenchyTokenizerFromRecipeRef(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "models ref",
			in:   "models--unsloth--Qwen3.5-122B-A10B-GGUF",
			want: "unsloth/Qwen3.5-122B-A10B-GGUF",
			ok:   true,
		},
		{
			name: "already hf",
			in:   "unsloth/Qwen3.5-122B-A10B-GGUF",
			want: "",
			ok:   false,
		},
		{
			name: "plain id",
			in:   "Qwen3.5-122B-A10B-GGUF",
			want: "",
			ok:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, ok := benchyTokenizerFromRecipeRef(tc.in)
			if ok != tc.ok {
				t.Fatalf("benchyTokenizerFromRecipeRef(%q) ok=%v, want %v", tc.in, ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("benchyTokenizerFromRecipeRef(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestProxyManager_ResolveBenchyTokenizerRecipeRefAutoFix(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"Qwen3.5-122B-A10B-GGUF": {
					UseModelName: "models--unsloth--Qwen3.5-122B-A10B-GGUF",
					Metadata: map[string]any{
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-llama-cpp",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("Qwen3.5-122B-A10B-GGUF", "")
	want := "unsloth/Qwen3.5-122B-A10B"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() recipe ref = %q, want %q", got, want)
	}
}

func TestResolveBenchyTokenizerLlamaAutoFix(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"qwen3-next-80b-a3b-thinking-gguf": {
					UseModelName: "unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF",
					Metadata: map[string]any{
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-llama-cpp",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("qwen3-next-80b-a3b-thinking-gguf", "")
	want := "unsloth/Qwen3-Next-80B-A3B-Thinking"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() = %q, want %q", got, want)
	}
}

func TestResolveBenchyTokenizerRespectsMetadataOverride(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"qwen3-next-80b-a3b-thinking-gguf": {
					UseModelName: "unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF",
					Metadata: map[string]any{
						"tokenizer": "Qwen/Qwen3-Next-80B-A3B-Thinking",
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-llama-cpp",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("qwen3-next-80b-a3b-thinking-gguf", "")
	want := "Qwen/Qwen3-Next-80B-A3B-Thinking"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() with metadata tokenizer = %q, want %q", got, want)
	}
}

func TestResolveBenchyTokenizerNonLlamaUnchanged(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"qwen3-next-80b-a3b-thinking-gguf": {
					UseModelName: "unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF",
					Metadata: map[string]any{
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-vllm-docker",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("qwen3-next-80b-a3b-thinking-gguf", "")
	want := "unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() non-llama = %q, want %q", got, want)
	}
}

func TestResolveBenchyTokenizerFixedEqualsModelFallsBackToAuto(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"Qwen3.5-122B-A10B-GGUF": {
					UseModelName: "models--unsloth--Qwen3.5-122B-A10B-GGUF",
					Metadata: map[string]any{
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-llama-cpp",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("Qwen3.5-122B-A10B-GGUF", "Qwen3.5-122B-A10B-GGUF")
	want := "unsloth/Qwen3.5-122B-A10B"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() fixed=model id = %q, want %q", got, want)
	}
}

func TestResolveBenchyTokenizerFixedRecipeRefNormalizes(t *testing.T) {
	pm := &ProxyManager{
		config: config.Config{
			Models: map[string]config.ModelConfig{
				"Qwen3.5-122B-A10B-GGUF": {
					UseModelName: "models--unsloth--Qwen3.5-122B-A10B-GGUF",
					Metadata: map[string]any{
						"recipe_ui": map[string]any{
							"backend_dir": "/home/csolutions_ai/swap-laboratories/backend/spark-llama-cpp",
						},
					},
				},
			},
		},
	}

	got := pm.resolveBenchyTokenizer("Qwen3.5-122B-A10B-GGUF", "models--unsloth--Qwen3.5-122B-A10B-GGUF")
	want := "unsloth/Qwen3.5-122B-A10B"
	if got != want {
		t.Fatalf("resolveBenchyTokenizer() fixed=recipe ref = %q, want %q", got, want)
	}
}
