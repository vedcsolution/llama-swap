#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_ROOT="${SCRIPT_DIR}/backend"
RECIPES_ROOT="${SCRIPT_DIR}/recipes"

usage() {
  cat <<USAGE
Usage: $0 <recipe-ref> [runner args...]

Dispatches a recipe to the correct backend runner (vLLM / llama.cpp) using
recipe metadata and backend discovery.
USAGE
}

trim_ext() {
  local v="$1"
  v="${v%.yaml}"
  v="${v%.yml}"
  printf '%s' "$v"
}

normalize_backend_hint() {
  local hint="${1:-}"
  hint="$(echo "$hint" | xargs)"
  if [[ -z "$hint" ]]; then
    return 1
  fi

  if [[ "$hint" = /* ]]; then
    if [[ -x "$hint/run-recipe.sh" ]]; then
      printf '%s' "$hint"
      return 0
    fi
    return 1
  fi

  local candidates=("$hint")
  if [[ "$hint" != spark-* ]]; then
    candidates+=("spark-$hint")
  fi

  local c
  for c in "${candidates[@]}"; do
    local dir="$BACKEND_ROOT/$c"
    if [[ -x "$dir/run-recipe.sh" ]]; then
      printf '%s' "$dir"
      return 0
    fi
  done
  return 1
}

meta_from_recipe() {
  local file="$1"
  python3 - "$file" <<'PY'
import sys
from pathlib import Path

try:
    import yaml
except Exception:
    print('|||||')
    raise SystemExit(0)

path = Path(sys.argv[1])
try:
    data = yaml.safe_load(path.read_text(encoding='utf-8')) or {}
except Exception:
    print('|||||')
    raise SystemExit(0)

backend = str(data.get('backend') or '').strip()
runtime = str(data.get('runtime') or '').strip()
recipe_ref = str(data.get('recipe_ref') or '').strip()
container = str(data.get('container') or '').strip()
command = data.get('command')
if command is None:
    command = ''
else:
    command = str(command).strip()
print(f"{backend}|{runtime}|{recipe_ref}|{container}|{command}")
PY
}

infer_backend_from_runtime() {
  local runtime="$(echo "${1:-}" | tr '[:upper:]' '[:lower:]' | xargs)"
  if [[ "$runtime" == *llama* ]]; then
    normalize_backend_hint "spark-llama-cpp" && return 0
  fi
  if [[ "$runtime" == *vllm* || "$runtime" == *trtllm* || "$runtime" == *nvidia* ]]; then
    normalize_backend_hint "spark-vllm-docker" && return 0
  fi
  return 1
}

infer_backend_from_container() {
  local container="$(echo "${1:-}" | tr '[:upper:]' '[:lower:]' | xargs)"
  if [[ -z "$container" ]]; then
    return 1
  fi
  if [[ "$container" == *llama* ]]; then
    normalize_backend_hint "spark-llama-cpp" && return 0
  fi
  if [[ "$container" == *vllm* || "$container" == *trtllm* || "$container" == *nvidia* ]]; then
    normalize_backend_hint "spark-vllm-docker" && return 0
  fi
  return 1
}

infer_backend_from_command() {
  local cmd="$(echo "${1:-}" | tr '[:upper:]' '[:lower:]' | xargs)"
  if [[ -z "$cmd" ]]; then
    return 1
  fi
  if [[ "$cmd" == *"vllm serve"* || "$cmd" == *"trtllm"* ]]; then
    normalize_backend_hint "spark-vllm-docker" && return 0
  fi
  if [[ "$cmd" == *"llama-server"* || "$cmd" == *"llama.cpp"* ]]; then
    normalize_backend_hint "spark-llama-cpp" && return 0
  fi
  return 1
}

find_recipe_in_backends() {
  local wanted="$1"
  local wanted_no_ext
  wanted_no_ext="$(trim_ext "$wanted")"
  local backend

  for backend in "$BACKEND_ROOT"/*; do
    [[ -d "$backend" ]] || continue
    [[ -x "$backend/run-recipe.sh" ]] || continue
    local recipes_dir="$backend/recipes"
    [[ -d "$recipes_dir" ]] || continue

    local p
    for p in "$recipes_dir/$wanted" "$recipes_dir/$wanted_no_ext.yaml" "$recipes_dir/$wanted_no_ext.yml"; do
      if [[ -f "$p" ]]; then
        local rel="${p#"$recipes_dir"/}"
        rel="$(trim_ext "$rel")"
        printf '%s|%s|%s\n' "$backend" "$p" "$rel"
        return 0
      fi
    done

    local found
    found="$(find "$recipes_dir" -type f \( -name "*.yaml" -o -name "*.yml" \) | while read -r f; do
      local rel="${f#"$recipes_dir"/}"
      rel="$(trim_ext "$rel")"
      local base
      base="$(basename "$rel")"
      if [[ "$base" == "$wanted_no_ext" || "$rel" == "$wanted_no_ext" ]]; then
        echo "$f|$rel"
      fi
    done | head -n 1 || true)"

    if [[ -n "$found" ]]; then
      local fpath="${found%%|*}"
      local rref="${found#*|}"
      printf '%s|%s|%s\n' "$backend" "$fpath" "$rref"
      return 0
    fi
  done

  return 1
}

if [[ $# -lt 1 ]]; then
  usage
  exit 1
fi

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi

recipe_input="$1"
shift
backend_dir=""
recipe_file=""
backend_recipe_ref=""

if [[ -f "$recipe_input" ]]; then
  recipe_file="$(cd "$(dirname "$recipe_input")" && pwd)/$(basename "$recipe_input")"
else
  input_no_ext="$(trim_ext "$recipe_input")"
  for p in "$RECIPES_ROOT/$recipe_input" "$RECIPES_ROOT/$input_no_ext.yaml" "$RECIPES_ROOT/$input_no_ext.yml"; do
    if [[ -f "$p" ]]; then
      recipe_file="$p"
      break
    fi
  done
fi

if [[ -n "$recipe_file" ]]; then
  rel_to_backend="${recipe_file#"$BACKEND_ROOT"/}"
  if [[ "$rel_to_backend" != "$recipe_file" && "$rel_to_backend" == */recipes/* ]]; then
    backend_name="${rel_to_backend%%/*}"
    backend_dir="$BACKEND_ROOT/$backend_name"
    backend_recipe_ref="${rel_to_backend#*/recipes/}"
    backend_recipe_ref="$(trim_ext "$backend_recipe_ref")"
  else
    IFS='|' read -r meta_backend meta_runtime meta_recipe_ref meta_container meta_command <<< "$(meta_from_recipe "$recipe_file")"
    if [[ -n "${meta_backend:-}" ]]; then
      backend_dir="$(normalize_backend_hint "$meta_backend" || true)"
    fi
    if [[ -z "$backend_dir" && -n "${meta_runtime:-}" ]]; then
      backend_dir="$(infer_backend_from_runtime "$meta_runtime" || true)"
    fi
    if [[ -z "$backend_dir" && -n "${meta_container:-}" ]]; then
      backend_dir="$(infer_backend_from_container "$meta_container" || true)"
    fi
    if [[ -z "$backend_dir" && -n "${meta_command:-}" ]]; then
      backend_dir="$(infer_backend_from_command "$meta_command" || true)"
    fi
    if [[ -n "${meta_recipe_ref:-}" ]]; then
      backend_recipe_ref="$meta_recipe_ref"
    fi
  fi
fi

if [[ -z "$backend_dir" ]]; then
  match="$(find_recipe_in_backends "$recipe_input" || true)"
  if [[ -n "$match" ]]; then
    IFS='|' read -r backend_dir recipe_file backend_recipe_ref <<< "$match"
  fi
fi

if [[ -z "$backend_dir" ]]; then
  echo "Error: no backend runner found for recipe '$recipe_input'." >&2
  exit 1
fi

runner="$backend_dir/run-recipe.sh"
if [[ ! -x "$runner" ]]; then
  echo "Error: backend runner not executable: $runner" >&2
  exit 1
fi

if [[ -n "$recipe_file" ]]; then
  recipes_dir="$backend_dir/recipes"
  if [[ "$recipe_file" == "$recipes_dir"/* ]]; then
    if [[ -z "$backend_recipe_ref" ]]; then
      backend_recipe_ref="${recipe_file#"$recipes_dir"/}"
      backend_recipe_ref="$(trim_ext "$backend_recipe_ref")"
    fi
  else
    # Keep full path for recipes stored outside backend/recipes (e.g. top-level recipes/).
    backend_recipe_ref="$recipe_file"
  fi
fi

if [[ -z "$backend_recipe_ref" ]]; then
  backend_recipe_ref="$(trim_ext "$recipe_input")"
fi

echo "[recipe-dispatch] backend=$backend_dir recipe=$backend_recipe_ref" >&2
exec "$runner" "$backend_recipe_ref" "$@"
