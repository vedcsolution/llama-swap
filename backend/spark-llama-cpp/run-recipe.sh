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

  # 0) Direct file path (e.g. top-level UI recipe dispatched by root runner)
  if [[ -f "$ref" ]]; then
    echo "$(cd "$(dirname "$ref")" && pwd)/$(basename "$ref")"
    return 0
  fi

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
port_from_cli="false"
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
      port_from_cli="true"
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
if model.startswith("models--") and "/" not in model:
    rest = model[len("models--"):]
    parts = rest.split("--")
    if len(parts) >= 2 and parts[0]:
        # Convert HF cache-style name to repo ID: models--org--name -> org/name
        model = parts[0] + "/" + "--".join(parts[1:])

container = str(data.get("container") or "llama-cpp-spark:last").strip()
defs = data.get("defaults") or {}
host = str(defs.get("host") or "0.0.0.0").strip()
port = str(defs.get("port") or "").strip()
n_gpu_layers = str(defs.get("n_gpu_layers") or "").strip()
ctx_size = str(defs.get("ctx_size") or "").strip()
temp = str(defs.get("temp") or "").strip()
top_p = str(defs.get("top_p") or "").strip()
top_k = str(defs.get("top_k") or "").strip()
min_p = str(defs.get("min_p") or "").strip()
gguf_file = str(data.get("gguf_file") or defs.get("gguf_file") or "").strip()

pairs = {
  "recipe_runtime": runtime,
  "recipe_model": model,
  "recipe_container": container,
  "recipe_host": host,
  "recipe_port": port,
  "recipe_n_gpu_layers": n_gpu_layers,
  "recipe_ctx_size": ctx_size,
  "recipe_temp": temp,
  "recipe_top_p": top_p,
  "recipe_top_k": top_k,
  "recipe_min_p": min_p,
  "recipe_gguf_file": gguf_file,
}

for k, v in pairs.items():
    print(f"{k}={shlex.quote(v)}")
PY
})"

# shellcheck disable=SC2086
eval "$recipe_env"
if [[ "$port_from_cli" != "true" && -n "${recipe_port:-}" ]]; then
  port="$recipe_port"
fi


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
cache_root="${LLAMA_CPP_CACHE_DIR:-/home/csolutions_ai/.cache/llama.cpp}"
hf_cache_root="${HF_HOME:-/home/csolutions_ai/.cache/huggingface}"
hf_cache_root="${hf_cache_root%/}"
mkdir -p "$cache_root" "$hf_cache_root"

resolve_local_hf_snapshot_model() {
  local model_ref="$1"
  local gguf_hint="$2"
  local hf_root="$3"

  python3 - "$model_ref" "$gguf_hint" "$hf_root" <<'PY'
import re
import sys
from pathlib import Path

model_ref_raw = sys.argv[1]
gguf_hint = sys.argv[2]
hf_root = sys.argv[3]

repo_ref, _, preset = model_ref_raw.partition(":")
repo_ref = repo_ref.strip()
preset = preset.strip()
if not repo_ref:
    raise SystemExit(1)

repo_dir = Path(hf_root) / "hub" / ("models--" + repo_ref.replace("/", "--"))
ref_file = repo_dir / "refs" / "main"
if not ref_file.is_file():
    raise SystemExit(1)

snapshot = ref_file.read_text(encoding="utf-8").strip()
if not snapshot:
    raise SystemExit(1)

snap_dir = repo_dir / "snapshots" / snapshot
if not snap_dir.is_dir():
    raise SystemExit(1)

if gguf_hint:
    hinted = snap_dir / gguf_hint
    if hinted.is_file():
        print(hinted)
        raise SystemExit(0)

search_root = snap_dir
if preset:
    search_root = snap_dir / preset
    if not search_root.is_dir():
        raise SystemExit(1)

candidates = [p for p in search_root.rglob("*.gguf") if p.is_file() and "mmproj" not in p.name.lower()]
if not candidates:
    raise SystemExit(1)

# If this is a sharded GGUF set, pass part 1 so llama.cpp can load the full shard set.
part1 = [p for p in candidates if re.search(r"-0*1-of-\d+\.gguf$", p.name, re.IGNORECASE)]
if part1:
    part1.sort(key=lambda p: p.stat().st_size, reverse=True)
    print(part1[0])
    raise SystemExit(0)

candidates.sort(key=lambda p: p.stat().st_size, reverse=True)
print(candidates[0])
PY
}

model_switch=(-hf "$hf_model")
model_source="hf"
model_arg_note="$hf_model"

if [[ "${LLAMA_CPP_PREFER_HF_SNAPSHOT:-1}" != "0" ]]; then
  local_model_host="$(resolve_local_hf_snapshot_model "$hf_model" "${recipe_gguf_file:-}" "$hf_cache_root" || true)"
  if [[ -n "$local_model_host" ]]; then
    local_model_container="/root/.cache/huggingface${local_model_host#$hf_cache_root}"
    model_switch=(-m "$local_model_container")
    model_source="hf-snapshot"
    model_arg_note="$local_model_container"
  fi
fi

safe_ref="$(echo "$recipe_ref" | tr '/:@.' '____' | tr -cd '[:alnum:]_-')"
container_name="llama_cpp_spark_${safe_ref}_${port}"

docker rm -f "$container_name" >/dev/null 2>&1 || true

cmd=(
  docker run
  --name "$container_name"
  --init
  --rm
  --gpus all
  -v "${cache_root}:/root/.cache/llama.cpp"
  -v "${hf_cache_root}:/root/.cache/huggingface"
  -e HF_HOME=/root/.cache/huggingface
  -e HUGGINGFACE_HUB_CACHE=/root/.cache/huggingface/hub
  -p "${port}:${port}"
  "$image"
  ${model_switch[@]}
  --host "$host"
  --port "$port"
)

if [[ -n "$recipe_n_gpu_layers" ]]; then
  cmd+=(--n-gpu-layers "$recipe_n_gpu_layers")
fi
if [[ -n "$recipe_ctx_size" ]]; then
  cmd+=(--ctx-size "$recipe_ctx_size")
fi
if [[ -n "$recipe_temp" ]]; then
  cmd+=(--temp "$recipe_temp")
fi
if [[ -n "$recipe_top_p" ]]; then
  cmd+=(--top-p "$recipe_top_p")
fi
if [[ -n "$recipe_top_k" ]]; then
  cmd+=(--top-k "$recipe_top_k")
fi
if [[ -n "$recipe_min_p" ]]; then
  cmd+=(--min-p "$recipe_min_p")
fi

cmd+=(--flash-attn on --jinja --no-webui)

if [[ ${#extra_args[@]} -gt 0 ]]; then
  cmd+=("${extra_args[@]}")
fi

echo "[llama-cpp-spark] recipe=$recipe_ref image=$image port=$port model_source=$model_source model_arg=$model_arg_note" >&2
exec "${cmd[@]}"
