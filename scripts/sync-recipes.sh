#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
RECIPES_DIR="${ROOT_DIR}/recipes"
AUTODISCOVER="${ROOT_DIR}/autodiscover.sh"

usage() {
  cat <<USAGE
Usage: $0 [--nodes "ip,ip"] [--file /path/to/recipe.yaml] [--delete] [--include-backend]

Syncs recipes to all autodiscovered nodes.

Options:
  --nodes "ip,ip"     Comma-separated list of nodes (overrides autodiscover).
  --file PATH         Sync a single recipe file (default: sync entire recipes/ dir).
  --delete            Delete remote recipes not present locally (mirror mode).
  --include-backend   Also sync backend/spark-vllm-docker/recipes.
USAGE
}

NODES_OVERRIDE=""
FILE_ONLY=""
DELETE_MODE=0
INCLUDE_BACKEND=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --nodes)
      NODES_OVERRIDE="${2:-}"; shift 2;;
    --file)
      FILE_ONLY="${2:-}"; shift 2;;
    --delete)
      DELETE_MODE=1; shift;;
    --include-backend)
      INCLUDE_BACKEND=1; shift;;
    -h|--help)
      usage; exit 0;;
    *)
      echo "Unknown option: $1" >&2; usage; exit 1;;
  esac
done

if [[ ! -d "$RECIPES_DIR" ]]; then
  echo "recipes directory not found: $RECIPES_DIR" >&2
  exit 1
fi

if [[ -n "$FILE_ONLY" ]]; then
  if [[ ! -f "$FILE_ONLY" ]]; then
    echo "recipe file not found: $FILE_ONLY" >&2
    exit 1
  fi
fi

get_nodes() {
  if [[ -n "$NODES_OVERRIDE" ]]; then
    echo "$NODES_OVERRIDE"
    return 0
  fi
  if [[ ! -f "$AUTODISCOVER" ]]; then
    echo "autodiscover.sh not found: $AUTODISCOVER" >&2
    exit 1
  fi
  # shellcheck source=/dev/null
  source "$AUTODISCOVER"
  detect_nodes >/dev/null 2>&1 || true
  detect_local_ip >/dev/null 2>&1 || true
  echo "${NODES_ARG:-}"
}

nodes_raw="$(get_nodes)"
if [[ -z "$nodes_raw" ]]; then
  echo "No nodes resolved. Use --nodes \"ip,ip\"" >&2
  exit 1
fi

IFS=, read -r -a NODES <<<"$nodes_raw"
LOCAL_IP="${LOCAL_IP:-}"

have_rsync=0
if command -v rsync >/dev/null 2>&1; then
  have_rsync=1
fi

sync_dir() {
  local src="$1"
  local dest="$2"
  local host="$3"
  if [[ $have_rsync -eq 1 ]]; then
    local rsync_flags=(-az)
    if [[ $DELETE_MODE -eq 1 ]]; then
      rsync_flags+=(--delete)
    fi
    rsync "${rsync_flags[@]}" -e "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new" "${src}/" "${host}:${dest}/"
  else
    # fallback: scp whole dir (no delete)
    scp -r -o BatchMode=yes -o StrictHostKeyChecking=accept-new "${src}" "${host}:$(dirname "$dest")/"
  fi
}

sync_file() {
  local file="$1"
  local host="$2"
  local dest_dir="$3"
  scp -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$file" "${host}:${dest_dir}/"
}

for node in "${NODES[@]}"; do
  node="$(echo "$node" | xargs)"
  [[ -z "$node" ]] && continue
  if [[ -n "$LOCAL_IP" && "$node" == "$LOCAL_IP" ]]; then
    continue
  fi

  echo "Syncing to $node ..."
  if [[ -n "$FILE_ONLY" ]]; then
    sync_file "$FILE_ONLY" "$node" "$RECIPES_DIR"
  else
    sync_dir "$RECIPES_DIR" "$RECIPES_DIR" "$node"
  fi

  if [[ $INCLUDE_BACKEND -eq 1 ]]; then
    backend_recipes="${ROOT_DIR}/backend/spark-vllm-docker/recipes"
    if [[ -d "$backend_recipes" ]]; then
      sync_dir "$backend_recipes" "$backend_recipes" "$node"
    fi
  fi

done

echo "Done."
