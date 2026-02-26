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



ensure_model_default_for_autogen_recipe() {
  local recipe_path="${1:-}"
  [[ -n "$recipe_path" && -f "$recipe_path" ]] || return 0

  python3 - "$recipe_path" <<'PY'
import re
import sys
from pathlib import Path

try:
    import yaml
except Exception:
    raise SystemExit(0)

path = Path(sys.argv[1])
try:
    raw_text = path.read_text(encoding='utf-8')
    data = yaml.safe_load(raw_text) or {}
except Exception:
    raise SystemExit(0)

if not isinstance(data, dict):
    raise SystemExit(0)

recipe_ref = str(data.get('recipe_ref') or '').strip()
if not recipe_ref.startswith('autogen/'):
    raise SystemExit(0)

changed = False

# If previous rewrites serialized multiline command into a quoted scalar,
# rewrite the file back to block scalar style for readability.
command = data.get('command')
if isinstance(command, str) and "\n" in command:
    if re.search(r'(?m)^command:\s*["\']', raw_text):
        changed = True

# Normalize legacy GGUF autogen command ordering to the current template.
runtime = str(data.get('runtime') or '').strip().lower()
if runtime == 'llama-cpp' and isinstance(command, str):
    if ('--host {host} --port {port}' in command and
        '--n-gpu-layers {n_gpu_layers}' in command and
        '--ctx-size {ctx_size}' in command and
        '--flash-attn on --jinja --no-webui' in command):
        normalized_command = """llama-server \\
    -hf {model} \\
    --ctx-size {ctx_size} \\
    --n-gpu-layers {n_gpu_layers} \\
    --port {port} --host {host} \\
    --flash-attn on \\
    --jinja \\
    --no-webui"""
        if command != normalized_command:
            data['command'] = normalized_command
            command = normalized_command
            changed = True

# Fix missing defaults.model in autogen recipes with {model} placeholder.
defaults = data.get('defaults')
if defaults is None or not isinstance(defaults, dict):
    defaults = {}

if isinstance(command, str) and '{model}' in command:
    existing_model = str(defaults.get('model') or '').strip()
    if not existing_model or existing_model.lower() == '<nil>':
        top_model = str(data.get('model') or '').strip()
        if top_model:
            defaults['model'] = top_model
            data['defaults'] = defaults
            changed = True

# Fix GGUF split hints: llama.cpp must start with -00001-of-xxxxx shard.
gguf_file = str(data.get('gguf_file') or '').strip()
if gguf_file:
    normalized = gguf_file.replace('\\', '/')
    shard = normalized.rsplit('/', 1)[-1]
    match = re.match(r'(?i)^(.+)-([0-9]{5})-of-([0-9]{5})\.gguf$', shard)
    if match:
        try:
            shard_idx = int(match.group(2))
        except Exception:
            shard_idx = 1
        if shard_idx > 1:
            first_shard = f"{match.group(1)}-00001-of-{match.group(3)}.gguf"
            prefix = normalized[: len(normalized) - len(shard)]
            fixed_gguf = prefix + first_shard
            if fixed_gguf != normalized:
                data['gguf_file'] = fixed_gguf
                changed = True

if not changed:
    raise SystemExit(0)

class _AutogenDumper(yaml.SafeDumper):
    pass


def _present_str(dumper, value):
    if "\n" in value:
        # Keep multiline fields (notably command) readable as block scalars.
        return dumper.represent_scalar('tag:yaml.org,2002:str', value, style='|')
    return dumper.represent_scalar('tag:yaml.org,2002:str', value)


_AutogenDumper.add_representer(str, _present_str)

header = "# Auto-generated by HF Models UI.\n# You can fine-tune this recipe from /ui/#/models.\n\n"
body = yaml.dump(data, Dumper=_AutogenDumper, sort_keys=False, allow_unicode=True, width=4096)
path.write_text(header + body, encoding='utf-8')
PY
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

find_recipe_in_catalog() {
  local wanted="$1"
  python3 - "$RECIPES_ROOT" "$wanted" <<'PY'
import sys
from pathlib import Path

try:
    import yaml
except Exception:
    print("")
    raise SystemExit(0)

root = Path(sys.argv[1])
wanted = sys.argv[2].strip()

def trim_ext(value: str) -> str:
    v = value
    if v.endswith(".yaml"):
        v = v[:-5]
    elif v.endswith(".yml"):
        v = v[:-4]
    return v

wanted_no_ext = trim_ext(wanted)
wanted_norm = wanted_no_ext.lower()

if not root.exists():
    print("")
    raise SystemExit(0)

for path in sorted(root.rglob("*")):
    if path.suffix.lower() not in {".yaml", ".yml"}:
        continue

    rel = trim_ext(path.relative_to(root).as_posix())
    base = rel.split("/")[-1]

    # Direct rel-path or basename matches first.
    if rel.lower() == wanted_norm or base.lower() == wanted_norm:
        print(str(path))
        raise SystemExit(0)

    try:
        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except Exception:
        continue

    recipe_ref = trim_ext(str(data.get("recipe_ref") or "").strip())
    if recipe_ref and recipe_ref.lower() == wanted_norm:
        print(str(path))
        raise SystemExit(0)

print("")
PY
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
  if [[ -z "$recipe_file" ]]; then
    recipe_file="$(find_recipe_in_catalog "$recipe_input" || true)"
  fi
fi

if [[ -n "$recipe_file" ]]; then
  ensure_model_default_for_autogen_recipe "$recipe_file"
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
  ensure_model_default_for_autogen_recipe "$recipe_file"
  # Always pass the resolved physical recipe file path to backend runners.
  # This keeps compatibility with recipe_ref values containing subdirectories
  # (e.g. autogen/foo) in backends that only resolve by basename when given a ref.
  backend_recipe_ref="$recipe_file"
fi

if [[ -z "$backend_recipe_ref" ]]; then
  backend_recipe_ref="$(trim_ext "$recipe_input")"
fi

echo "[recipe-dispatch] backend=$backend_dir recipe=$backend_recipe_ref" >&2
exec "$runner" "$backend_recipe_ref" "$@"
