#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECIPES_DIR="$SCRIPT_DIR/recipes"

list_recipe_refs() {
  [[ -d "$RECIPES_DIR" ]] || return 0
  find "$RECIPES_DIR" -type f \( -name "*.yaml" -o -name "*.yml" \) | sort | while read -r f; do
    local rel="${f#"$RECIPES_DIR"/}"
    rel="${rel%.yaml}"
    rel="${rel%.yml}"
    echo "$rel"
  done
}

usage() {
  cat <<USAGE
Usage: $0 <recipe-ref> [--solo] [-n nodes] [--tp N] [--port PORT] [extra llama-server args...]

Supported recipe refs:
$(list_recipe_refs | sed 's/^/  - /')

Notes:
  - llama.cpp backend here is solo-oriented (nodes/tp are accepted but ignored).
  - Recipe ref can be full relative path (e.g. unsloth/Qwen3-...) or unique basename.

Environment overrides:
  - LLAMA_CPP_SPARK_IMAGE  (override recipe container image)
USAGE
}

resolve_recipe_file() {
  local ref="$1"

  # 1) Exact relative path candidates
  local candidates=(
    "$RECIPES_DIR/$ref"
    "$RECIPES_DIR/$ref.yaml"
    "$RECIPES_DIR/$ref.yml"
  )
  local c
  for c in "${candidates[@]}"; do
    if [[ -f "$c" ]]; then
      echo "$c"
      return 0
    fi
  done

  # 2) Basename fallback (only when caller used short id)
  local base="$ref"
  base="${base%.yaml}"
  base="${base%.yml}"

  local matches=()
  while IFS= read -r f; do
    matches+=("$f")
  done < <(find "$RECIPES_DIR" -type f \( -name "*.yaml" -o -name "*.yml" \) -print | while read -r f; do
    b="$(basename "$f")"
    b="${b%.yaml}"
    b="${b%.yml}"
    [[ "$b" == "$base" ]] && echo "$f"
  done)

  if [[ ${#matches[@]} -eq 1 ]]; then
    echo "${matches[0]}"
    return 0
  fi

  if [[ ${#matches[@]} -gt 1 ]]; then
    echo "Error: recipe ref '$ref' is ambiguous; use full relative path." >&2
    printf 'Matches:\n' >&2
    local m
    for m in "${matches[@]}"; do
      rel="${m#"$RECIPES_DIR"/}"
      rel="${rel%.yaml}"
      rel="${rel%.yml}"
      echo "  - $rel" >&2
    done
    return 2
  fi

  return 1
}

if [[ $# -lt 1 ]]; then
  usage
  exit 1
fi

case "${1:-}" in
  --help|-h)
    usage
    exit 0
    ;;
  --list)
    list_recipe_refs
    exit 0
    ;;
esac

recipe_ref="$1"
shift

port="8080"
solo="false"
nodes=""
tp=""
extra_args=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --solo)
      solo="true"
      shift
      ;;
    -n|--nodes)
      [[ $# -ge 2 ]] || { echo "Error: $1 requires a value" >&2; exit 1; }
      nodes="$2"
      shift 2
      ;;
    --tp)
      [[ $# -ge 2 ]] || { echo "Error: --tp requires a value" >&2; exit 1; }
      tp="$2"
      shift 2
      ;;
    --port)
      [[ $# -ge 2 ]] || { echo "Error: --port requires a value" >&2; exit 1; }
      port="$2"
      shift 2
      ;;
    *)
      extra_args+=("$1")
      shift
      ;;
  esac
done

recipe_file="$(resolve_recipe_file "$recipe_ref" || true)"
if [[ -z "$recipe_file" ]]; then
  echo "Error: unsupported recipe '$recipe_ref'." >&2
  echo "Supported refs:" >&2
  list_recipe_refs | sed 's/^/  - /' >&2
  exit 1
fi

recipe_env="$({
python3 - "$recipe_file" <<'PY'
import shlex
import sys
import yaml

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    data = yaml.safe_load(f) or {}

runtime = str(data.get("runtime") or "llama-cpp").strip()
model = str(data.get("model") or "").strip()
container = str(data.get("container") or "llama-cpp-spark:last").strip()
defs = data.get("defaults") or {}
host = str(defs.get("host") or "0.0.0.0").strip()
n_gpu_layers = str(defs.get("n_gpu_layers") or "").strip()
ctx_size = str(defs.get("ctx_size") or "").strip()

pairs = {
  "recipe_runtime": runtime,
  "recipe_model": model,
  "recipe_container": container,
  "recipe_host": host,
  "recipe_n_gpu_layers": n_gpu_layers,
  "recipe_ctx_size": ctx_size,
}

for k, v in pairs.items():
    print(f"{k}={shlex.quote(v)}")
PY
})"

# shellcheck disable=SC2086
eval "$recipe_env"

if [[ "$recipe_runtime" != "llama-cpp" && "$recipe_runtime" != "llama-cpp-spark" ]]; then
  echo "Error: unsupported runtime '$recipe_runtime' in $recipe_file (expected llama-cpp)." >&2
  exit 1
fi

if [[ -z "$recipe_model" ]]; then
  echo "Error: recipe model is empty in $recipe_file" >&2
  exit 1
fi

if [[ -n "$nodes" && "$solo" != "true" ]]; then
  echo "Warning: llama.cpp backend is solo-only; ignoring nodes='$nodes'." >&2
fi
if [[ -n "$tp" && "$tp" != "1" ]]; then
  echo "Warning: llama.cpp does not use tensor parallel in this runner; ignoring --tp $tp." >&2
fi

image="${LLAMA_CPP_SPARK_IMAGE:-$recipe_container}"
host="${recipe_host:-0.0.0.0}"
hf_model="$recipe_model"

safe_ref="$(echo "$recipe_ref" | tr '/:@.' '____' | tr -cd '[:alnum:]_-')"
container_name="llama_cpp_spark_${safe_ref}_${port}"

docker rm -f "$container_name" >/dev/null 2>&1 || true

cmd=(
  docker run
  --name "$container_name"
  --init
  --rm
  --gpus all
  -p "${port}:${port}"
  "$image"
  -hf "$hf_model"
  --host "$host"
  --port "$port"
)

if [[ -n "$recipe_n_gpu_layers" ]]; then
  cmd+=(--n-gpu-layers "$recipe_n_gpu_layers")
fi
if [[ -n "$recipe_ctx_size" ]]; then
  cmd+=(--ctx-size "$recipe_ctx_size")
fi

cmd+=(--flash-attn on --jinja --no-webui)

if [[ ${#extra_args[@]} -gt 0 ]]; then
  cmd+=("${extra_args[@]}")
fi

echo "[llama-cpp-spark] recipe=$recipe_ref image=$image port=$port model=$hf_model" >&2
exec "${cmd[@]}"
