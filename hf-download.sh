#!/usr/bin/env bash
set -euo pipefail

HUB_PATH="${HF_HUB_PATH:-$HOME/.cache/huggingface/hub}"
SSH_COMMON_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)
RSYNC_SSH_CMD="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15"

COPY_HOSTS=()
SSH_USER="${USER:-}"
PARALLEL_COPY=false
MODEL_NAME=""
MODEL_FORMAT="safetensors"
QUANTIZATION=""
CUSTOM_INCLUDE=""

usage() {
    cat <<USAGE
Usage: $0 [OPTIONS] <model-name>

Options:
  --format <gguf|safetensors>  Download format filter (default: safetensors)
  --quantization <value>       Quantization hint (e.g. Q8_0, Q4_K_M, 4bit)
  --include <glob>             Custom include glob (overrides format/quantization)
  -c, --copy-to <hosts>        Copy model cache directory to peers (comma or space-separated)
      --copy-parallel          Copy to peers in parallel
  -u, --user <user>            SSH user for copy (default: current user)
  -h, --help                   Show this help

Examples:
  $0 --format gguf --quantization Q8_0 unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF
  $0 --format safetensors --quantization 4bit deepseek-ai/DeepSeek-R1
  $0 --include "*Q6_K*.gguf" unsloth/Qwen3-Next-80B-A3B-Thinking-GGUF -c --copy-parallel
USAGE
}

add_copy_hosts() {
    local token part
    for token in "$@"; do
        IFS=',' read -ra PARTS <<< "$token"
        for part in "${PARTS[@]}"; do
            part="${part//[[:space:]]/}"
            if [[ -n "$part" ]]; then
                COPY_HOSTS+=("$part")
            fi
        done
    done
}

build_local_manifest() {
    local dir="$1"
    (
        cd "$dir" &&
        {
            find . -type f -printf 'F|%P|%s\n'
            find . -type l -printf 'L|%P|%l\n'
        } | LC_ALL=C sort
    )
}

build_remote_manifest() {
    local host="$1"
    local dir="$2"
    ssh "${SSH_COMMON_OPTS[@]}" "${SSH_USER}@${host}" "cd \"$dir\" 2>/dev/null && { find . -type f -printf 'F|%P|%s\n'; find . -type l -printf 'L|%P|%l\n'; } | LC_ALL=C sort"
}

diagnose_remote_permissions() {
    local host="$1"
    local model_dir="$2"
    local model_base remote_dir

    model_base="$(basename "$model_dir")"
    remote_dir="${HUB_PATH}/${model_base}"

    echo "Ownership diagnostics on ${host} (first 20 mismatches):"
    ssh "${SSH_COMMON_OPTS[@]}" "${SSH_USER}@${host}" \
        "find \"$remote_dir\" \\( ! -user \"$SSH_USER\" -o ! -group \"$SSH_USER\" \\) -printf '%u:%g %p\n' 2>/dev/null | head -n 20" || true
    echo "Suggested fix on ${host}: sudo chown -R ${SSH_USER}:${SSH_USER} \"$remote_dir\""
}

verify_remote_model_copy() {
    local host="$1"
    local model_dir="$2"
    local model_base remote_dir local_manifest remote_manifest

    model_base="$(basename "$model_dir")"
    remote_dir="${HUB_PATH}/${model_base}"

    if ! ssh "${SSH_COMMON_OPTS[@]}" "${SSH_USER}@${host}" "test -d \"$remote_dir\""; then
        return 1
    fi

    local_manifest="$(mktemp)"
    remote_manifest="$(mktemp)"

    if ! build_local_manifest "$model_dir" >"$local_manifest"; then
        rm -f "$local_manifest" "$remote_manifest"
        return 1
    fi
    if ! build_remote_manifest "$host" "$remote_dir" >"$remote_manifest"; then
        rm -f "$local_manifest" "$remote_manifest"
        return 1
    fi

    if diff -q "$local_manifest" "$remote_manifest" >/dev/null 2>&1; then
        rm -f "$local_manifest" "$remote_manifest"
        return 0
    fi

    echo "Verification mismatch for $host (local vs remote manifest differ)."
    diff -u "$local_manifest" "$remote_manifest" | sed -n '1,80p' || true
    rm -f "$local_manifest" "$remote_manifest"
    return 1
}

copy_model_to_host() {
    local host="$1"
    local model_name="$2"
    local model_dir="$3"
    local model_base rsync_rc

    echo "Copying model '$model_name' to ${SSH_USER}@${host}..."
    local host_copy_start host_copy_end host_copy_time
    host_copy_start=$(date +%s)
    model_base="$(basename "$model_dir")"

    if ! ssh "${SSH_COMMON_OPTS[@]}" "${SSH_USER}@${host}" "mkdir -p \"$HUB_PATH\""; then
        echo "Copy to $host failed: could not create/access $HUB_PATH on remote host."
        return 1
    fi

    set +e
    rsync -a --partial --human-readable --info=progress2 --no-owner --no-group --no-perms --omit-dir-times --no-times \
        --exclude '.no_exist/' \
        -e "$RSYNC_SSH_CMD" \
        "$model_dir/" "${SSH_USER}@${host}:$HUB_PATH/$model_base/"
    rsync_rc=$?
    set -e

    if [[ "$rsync_rc" -eq 0 ]]; then
        host_copy_end=$(date +%s)
        host_copy_time=$((host_copy_end - host_copy_start))
        printf "Copy to %s completed in %02d:%02d:%02d\n" "$host" $((host_copy_time/3600)) $((host_copy_time%3600/60)) $((host_copy_time%60))
    elif [[ "$rsync_rc" -eq 23 ]]; then
        echo "Warning: rsync returned code 23 for $host. Verifying copied files..."
        if verify_remote_model_copy "$host" "$model_dir"; then
            host_copy_end=$(date +%s)
            host_copy_time=$((host_copy_end - host_copy_start))
            printf "Copy to %s verified in %02d:%02d:%02d (rsync code 23 tolerated)\n" "$host" $((host_copy_time/3600)) $((host_copy_time%3600/60)) $((host_copy_time%60))
        else
            diagnose_remote_permissions "$host" "$model_dir"
            echo "Copy to $host failed: rsync code 23 and verification did not pass."
            return 1
        fi
    else
        echo "Copy to $host failed (rsync exit $rsync_rc)."
        return 1
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --format)
            shift
            [[ $# -eq 0 ]] && { echo "Error: --format requires a value" >&2; usage; exit 1; }
            MODEL_FORMAT="$(echo "$1" | tr '[:upper:]' '[:lower:]')"
            ;;
        --quantization)
            shift
            [[ $# -eq 0 ]] && { echo "Error: --quantization requires a value" >&2; usage; exit 1; }
            QUANTIZATION="$(echo "$1" | xargs)"
            ;;
        --include)
            shift
            [[ $# -eq 0 ]] && { echo "Error: --include requires a value" >&2; usage; exit 1; }
            CUSTOM_INCLUDE="$1"
            ;;
        -c|--copy-to|--copy-to-host|--copy-to-hosts)
            shift
            while [[ $# -gt 0 && "$1" != -* ]]; do
                add_copy_hosts "$1"
                shift
            done

            if [[ ${#COPY_HOSTS[@]} -eq 0 ]]; then
                echo "No hosts specified. Using autodiscovery..."
                source "$(dirname "$0")/autodiscover.sh"
                detect_nodes
                if [[ $? -ne 0 ]]; then
                    echo "Error: Autodiscovery failed."
                    exit 1
                fi
                if [[ ${#PEER_NODES[@]} -gt 0 ]]; then
                    COPY_HOSTS=("${PEER_NODES[@]}")
                fi
                if [[ ${#COPY_HOSTS[@]} -eq 0 ]]; then
                    echo "Error: Autodiscovery found no other nodes."
                    exit 1
                fi
                echo "Autodiscovered hosts: ${COPY_HOSTS[*]}"
            fi
            continue
            ;;
        --copy-parallel)
            PARALLEL_COPY=true
            ;;
        -u|--user)
            shift
            [[ $# -eq 0 ]] && { echo "Error: --user requires a value" >&2; usage; exit 1; }
            SSH_USER="$1"
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            if [[ -z "$MODEL_NAME" ]]; then
                MODEL_NAME="$1"
            else
                echo "Error: Unknown parameter: $1" >&2
                usage
                exit 1
            fi
            ;;
    esac
    shift
done

if [[ -z "$MODEL_NAME" ]]; then
    echo "Error: Model name is required." >&2
    usage
    exit 1
fi

case "$MODEL_FORMAT" in
    gguf|safetensors)
        ;;
    safetensor)
        MODEL_FORMAT="safetensors"
        ;;
    *)
        echo "Error: --format must be gguf or safetensors (got: $MODEL_FORMAT)" >&2
        exit 1
        ;;
esac

if [[ "$MODEL_NAME" =~ [[:space:]] ]]; then
    echo "Error: Model name must not contain whitespace." >&2
    exit 1
fi
if [[ "$QUANTIZATION" =~ [[:space:]] ]]; then
    echo "Error: --quantization must not contain whitespace." >&2
    exit 1
fi

if ! command -v uvx >/dev/null 2>&1; then
    echo "Error: 'uvx' command not found." >&2
    echo "Install uv first: curl -LsSf https://astral.sh/uv/install.sh | sh" >&2
    exit 1
fi

INCLUDE_PATTERNS=()
if [[ -n "$CUSTOM_INCLUDE" ]]; then
    INCLUDE_PATTERNS+=("$CUSTOM_INCLUDE")
else
    if [[ "$MODEL_FORMAT" == "gguf" ]]; then
        if [[ -n "$QUANTIZATION" ]]; then
            INCLUDE_PATTERNS+=("*${QUANTIZATION}*.gguf")
        else
            INCLUDE_PATTERNS+=("*.gguf")
        fi
    else
        if [[ -n "$QUANTIZATION" ]]; then
            INCLUDE_PATTERNS+=("*${QUANTIZATION}*.safetensors")
        else
            INCLUDE_PATTERNS+=("*.safetensors")
        fi
    fi
fi

DOWNLOAD_ARGS=("$MODEL_NAME")
for pattern in "${INCLUDE_PATTERNS[@]}"; do
    DOWNLOAD_ARGS+=("--include" "$pattern")
done

START_TIME=$(date +%s)

echo "Downloading model '$MODEL_NAME' (format=$MODEL_FORMAT quantization=${QUANTIZATION:-none})"
echo "Include patterns: ${INCLUDE_PATTERNS[*]}"
DOWNLOAD_START=$(date +%s)
if uvx hf download "${DOWNLOAD_ARGS[@]}"; then
    DOWNLOAD_END=$(date +%s)
    DOWNLOAD_TIME=$((DOWNLOAD_END - DOWNLOAD_START))
    printf "Download completed in %02d:%02d:%02d\n" $((DOWNLOAD_TIME/3600)) $((DOWNLOAD_TIME%3600/60)) $((DOWNLOAD_TIME%60))
else
    echo "Error: Failed to download model '$MODEL_NAME'." >&2
    exit 1
fi

MODEL_DIR=""
ORG=""
MODEL="$MODEL_NAME"
if [[ "$MODEL_NAME" == */* ]]; then
    ORG="${MODEL_NAME%%/*}"
    MODEL="${MODEL_NAME##*/}"
fi

if [[ -d "$HUB_PATH" ]]; then
    if [[ -n "$ORG" ]]; then
        MODEL_DIR=$(find "$HUB_PATH" -maxdepth 1 -type d -name "models--${ORG}--${MODEL}*" | sort | tail -n 1)
    else
        MODEL_DIR=$(find "$HUB_PATH" -maxdepth 1 -type d -name "models--${MODEL}*" | sort | tail -n 1)
    fi
fi

if [[ -z "$MODEL_DIR" || ! -d "$MODEL_DIR" ]]; then
    echo "Error: Could not find downloaded model directory in $HUB_PATH" >&2
    exit 1
fi

echo "Model directory: $MODEL_DIR"

COPY_TIME=0
if [[ ${#COPY_HOSTS[@]} -gt 0 ]]; then
    echo ""
    echo "Copying model to ${#COPY_HOSTS[@]} host(s): ${COPY_HOSTS[*]}"
    [[ "$PARALLEL_COPY" == true ]] && echo "Parallel copy enabled."
    COPY_START=$(date +%s)

    if [[ "$PARALLEL_COPY" == true ]]; then
        PIDS=()
        for host in "${COPY_HOSTS[@]}"; do
            copy_model_to_host "$host" "$MODEL_NAME" "$MODEL_DIR" &
            PIDS+=("$!")
        done
        COPY_FAILURE=0
        for pid in "${PIDS[@]}"; do
            if ! wait "$pid"; then
                COPY_FAILURE=1
            fi
        done
        if [[ "$COPY_FAILURE" -ne 0 ]]; then
            echo "One or more copies failed." >&2
            exit 1
        fi
    else
        for host in "${COPY_HOSTS[@]}"; do
            copy_model_to_host "$host" "$MODEL_NAME" "$MODEL_DIR"
        done
    fi

    COPY_END=$(date +%s)
    COPY_TIME=$((COPY_END - COPY_START))
    echo ""
    echo "Copy complete."
else
    echo "No host specified, skipping copy."
fi

END_TIME=$(date +%s)
TOTAL_TIME=$((END_TIME - START_TIME))

echo ""
echo "========================================="
echo "         TIMING STATISTICS"
echo "========================================="
echo "Download:   $(printf '%02d:%02d:%02d' $((DOWNLOAD_TIME/3600)) $((DOWNLOAD_TIME%3600/60)) $((DOWNLOAD_TIME%60)))"
if [[ "$COPY_TIME" -gt 0 ]]; then
    echo "Copy:      $(printf '%02d:%02d:%02d' $((COPY_TIME/3600)) $((COPY_TIME%3600/60)) $((COPY_TIME%60)))"
fi
echo "Total:     $(printf '%02d:%02d:%02d' $((TOTAL_TIME/3600)) $((TOTAL_TIME%3600/60)) $((TOTAL_TIME%60)))"
echo "========================================="
echo "Done downloading $MODEL_NAME."
