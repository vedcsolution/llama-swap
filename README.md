# Swap Laboratories

Swap Laboratories is a fork of `llama-swap` focused on operating recipe-based inference clusters from the `spark-*` backend repos (`vLLM`, `TRT-LLM`, `SGLang`) with a practical web control plane.

Upstream project: [mostlygeek/llama-swap](https://github.com/mostlygeek/llama-swap)

## What This Fork Adds

This repository keeps the core model-swap proxy behavior and adds an operations layer tailored for multi-node recipe workflows:

- Backend selector in UI (`Backend` page) with persisted override and backend discovery from `swap-laboratories/backend/*` (default active backend: `~/swap-laboratories/backend/spark-vllm-docker`).
- Recipe Manager in UI (`Models` page) to create/update/delete managed models from backend recipes.
- Cluster diagnostics page (`Cluster`) using backend `autodiscover.sh` + SSH checks.
- YAML Config Editor (`Editor`) with CodeMirror syntax highlighting + validation on save.
- llama-benchy integration in UI and API, including intelligence plugins.
- Extra backend actions from UI:
  - Git sync (`git_pull`, `git_pull_rebase`)
  - HF model download (`download_hf_model` via `hf-download.sh`)
  - vLLM builds (`build_vllm`, `build_mxfp4`, `build_vllm_12_0f`)
  - TRT-LLM and NVIDIA image management (`pull_*_image`, `update_*_image`)
  - llama.cpp image build (`build_llamacpp`)
- Home-safe path rendering in UI (`~` instead of hardcoded absolute home path in labels).

## Core Behavior (Inherited + Extended)

- OpenAI-compatible request proxy with model-based routing and hot swapping.
- Optional model groups for exclusive/swap behavior.
- Per-model lifecycle (`load`, `unload`, `ttl`, `cmdStop`, health checks).
- Unified activity/log stream in the UI via SSE (`/api/events`).

## Requirements

- Go `1.24+`
- Node.js + npm (for Svelte UI build)
- Linux environment recommended for cluster/backend operations
- Docker and SSH access for recipe backends (depends on your backend repo)

## Quick Start (From Source)

```bash
git clone https://github.com/vedcsolution/llama-swap.git
cd llama-swap

# Build UI assets
make ui

# Build binary
go build -o build/llama-swap .

# Start
./build/llama-swap --config ./config.yaml --watch-config --listen 0.0.0.0:8080
```

Then open:

- UI: `http://127.0.0.1:8080/ui`
- API health: `http://127.0.0.1:8080/health`

## Backend + Recipe Workflow

### 1) Pick backend root

Use the UI `Backend` tab or env var:

```bash
export LLAMA_SWAP_RECIPES_BACKEND_DIR="$HOME/swap-laboratories/backend/spark-vllm-docker"
```

Backend selection precedence is:

1. persisted override (`LLAMA_SWAP_RECIPES_BACKEND_OVERRIDE_FILE`)
2. `LLAMA_SWAP_RECIPES_BACKEND_DIR`
3. default `~/swap-laboratories/backend/spark-vllm-docker`

A valid backend directory must contain:

- `run-recipe.sh`
- `recipes/`

### 2) Manage recipe models

Use `Models -> Recipe Manager` to generate model entries in `config.yaml`.

Managed entries are written with metadata under:

- `metadata.recipe_ui.*`

When saving from Recipe Manager, this fork also ensures macros required for portability:

- `user_home`
- `spark_root`
- `recipe_runner`
- `llama_root`

### 3) Start/stop models

- Per-model load/unload from `Models` page.
- `Stop Cluster` triggers immediate local unload + backend `launch-cluster.sh stop`.
- For control-plane-only deployments (no local inference), enable agent mode:
  - set `LLAMA_SWAP_CLUSTER_EXEC_MODE=agent`
  - run `llama-swap-agent` on each node
  - use `cluster-inventory.yaml` (see `docs/agent-cluster.md`)

## UI Sections

- `Playground`: chat/image/speech/transcription client against current API.
- `Models`: model states, load/unload, Recipe Manager, Benchy.
- `Activity`: request/token activity history.
- `Logs`: proxy + upstream logs.
- `Cluster`: autodiscovery + SSH/port 22 health matrix.
- `Backend`: backend selection + backend actions.
- `Editor`: live `config.yaml` code editor with validation.

## Benchy Integration

This fork exposes benchy in UI and API:

- `POST /api/benchy`
- `GET /api/benchy/:id`
- `POST /api/benchy/:id/cancel`

Supported options include:

- `tokenizer`, `baseUrl`, `pp`, `tg`, `depth`, `concurrency`, `runs`, `latencyMode`
- `noCache`, `noWarmup`, `adaptPrompt`, `enablePrefixCaching`, `trustRemoteCode`
- Intelligence mode: `enableIntelligence`, `intelligencePlugins`, `allowCodeExec`, `datasetCacheDir`, `outputDir`, `maxConcurrent`

Runner resolution order:

1. `LLAMA_BENCHY_CMD`
2. `uvx --from ... llama-benchy`
3. `llama-benchy`

Intelligence plugin source:

- `christopherowen/llama-benchy` (`@intelligence`)

## API Surface (Ops-focused)

- `POST /api/models/unload`
- `POST /api/models/unload/:model`
- `POST /api/cluster/stop`
- `GET /api/cluster/status`
- `POST /api/cluster/dgx/update`
- `GET /api/images/docker`
- `POST /api/images/docker/update`
- `POST /api/images/docker/delete`
- `GET /api/config/editor`
- `PUT /api/config/editor`
- `GET /api/recipes/state`
- `GET /api/recipes/backend`
- `PUT /api/recipes/backend`
- `GET /api/recipes/containers`
- `GET /api/recipes/selected-container`
- `PUT /api/recipes/selected-container`
- `POST /api/recipes/backend/action`
- `GET /api/recipes/backend/action-status`
- `GET /api/recipes/backend/hf-models`
- `PUT /api/recipes/backend/hf-models/path`
- `DELETE /api/recipes/backend/hf-models`
- `POST /api/recipes/models`
- `DELETE /api/recipes/models/:id`
- `GET /api/recipes/source`
- `PUT /api/recipes/source`
- `POST /api/recipes/source/create`
- `POST /api/benchy`
- `GET /api/benchy/:id`
- `POST /api/benchy/:id/cancel`
- `GET /api/events`
- `GET /api/metrics`
- `GET /api/version`
- `GET /api/captures/:id`

## Environment Variables

### Recipe/backend paths

- `LLAMA_SWAP_RECIPES_BACKEND_DIR`: active backend root.
- `LLAMA_SWAP_RECIPES_BACKEND_OVERRIDE_FILE`: file used to persist backend override.
- `LLAMA_SWAP_RECIPES_DIR`: explicit recipes catalog directory (when set, searched first).
- `LLAMA_SWAP_LOCAL_RECIPES_DIR`: extra local recipe directory.
- `LLAMA_SWAP_CLUSTER_AUTODISCOVER_PATH`: override autodiscover script path.
- `LLAMA_SWAP_CLUSTER_EXEC_MODE`: execution mode (`local` default, `agent` for agent-managed nodes).
- `LLAMA_SWAP_CLUSTER_INVENTORY_FILE`: optional explicit inventory path (auto-detects `cluster-inventory.yaml`).
- `LLAMA_SWAP_CLUSTER_HEAD_NODE`: optional head node override (id/data/control/proxy IP).
- `LLAMA_SWAP_CLUSTER_DEFAULT_SSH_USER`: default ssh user used in inventory normalization.
- `LLAMA_SWAP_CLUSTER_AGENT_REMOTE_ROOT`: remote repo root used by agent mode recipe commands (default `$HOME/swap-laboratories`).
- `LLAMA_SWAP_AGENT_DEFAULT_PORT`: default agent port (default `19090`).
- `LLAMA_SWAP_AGENT_TOKEN_FILE`: optional bearer token file for control-host -> agent auth.
- `LLAMA_SWAP_CLUSTER_RDMA_REQUIRED`: cluster RDMA requirement flag (`1/true`).
- `LLAMA_SWAP_CLUSTER_RDMA_ETH_IF`: required ETH interface name.
- `LLAMA_SWAP_CLUSTER_RDMA_IB_IF`: required IB interface list (comma separated).
- `LLAMA_SWAP_HF_DOWNLOAD_SCRIPT`: override `hf-download.sh` path used by backend actions.
- `LLAMA_SWAP_HF_HUB_PATH`: Hugging Face hub cache base path.
- `LLAMA_SWAP_HF_HUB_PATH_OVERRIDE_FILE`: file used to persist HF hub path override.
- `LLAMA_SWAP_CONFIG_PATH`: fallback config path if not started with `--config`.

### Benchy

- `LLAMA_BENCHY_CMD`: explicit benchy runner command.
- `LLAMA_BENCHY_DISABLE`: disable benchy API (`1`/`true`).
- `LLAMA_BENCHY_OUTPUT_DIR`: default output directory.
- `LLAMA_SWAP_BENCHY_PY_SHIM_DIR`: optional py shim dir.
- `LLAMA_SWAP_SWEBENCH_TEXT_COMPAT`: SWE-bench text compatibility toggle.

## Security Notes (This Fork)

- No SSH private keys are stored in this repository.
- Cluster operations use the system `ssh` client and your local user credentials/agent.
- Secrets should be passed via environment variables and macros (for example `${env.HF_TOKEN}`, `${env.OPENROUTER_API_KEY}`).
- Avoid committing local `config.yaml` values that include private hostnames, tokens, or internal topology details.

## Marlin-sm12x Image Build Helper

`scripts/build-vllm-marlin-sm12x.sh` is not included in this fork.

## NVMe-oF Canary Toolkit

This fork includes starter scripts to harden NVMe-oF initiator connectivity and network tuning without changing current model paths:

- `scripts/nvmeof-initiator-canary.sh`
- `scripts/net-tune-canary.sh`
- `scripts/install-nvmeof-canary-units.sh`

Systemd templates:

- `scripts/systemd/nvmeof-connect@.service`
- `scripts/systemd/net-tune-canary.service`

These help apply canary settings (`keep-alive`, `ctrl-loss`, reconnect delay, queue size, sysctl snapshot/rollback) and keep reconnect order at boot.

The `Cluster` UI now also shows a storage baseline matrix (per-node path presence) to highlight potential duplicated local caches and track optimization toward a shared read path.

Related pending plans:

- docs/pending/nvmeof-overlayfs-cache-strategy.md
- docs/pending/prefetch-sidecar-ttft-and-nvmeof-canary.md

## Troubleshooting

### Benchy: `plugin 'swebench_verified' requires allowCodeExec=true`

Enable `allow-code-exec` when running EvalPlus / SWE-bench / Terminal-Bench plugins.

### Benchy: `PermissionError` in `~/.cache/huggingface/datasets/*.lock`

Fix permissions on existing cache (no new cache dir required):

```bash
sudo chown -R "$USER:$USER" ~/.cache/huggingface/datasets
```

### Benchy warning: `PyTorch was not found...`

This usually comes from local tokenizer/tooling path in benchy subprocess; it does not necessarily mean your serving backend lacks PyTorch.

### HF rate-limit warning

Set `HF_TOKEN` to avoid unauthenticated hub limits.

### Long sequence warning (`... > 1024`)

Comes from tokenizer config metadata in some models; verify actual server-side max context and tokenizer behavior for your recipe.

## Development Notes

- Build UI only: `make ui`
- Run tests: `make test`
- Full proxy tests: `make test-all`

Primary config references:

- `config.example.yaml`
- `docs/configuration.md`

---

If you are looking for the original generic project README, see upstream:
[mostlygeek/llama-swap](https://github.com/mostlygeek/llama-swap)
