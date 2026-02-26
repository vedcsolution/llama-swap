# Agent Cluster Mode

`llama-swap` can run on a control host that does not perform inference by using node agents.

## 1. Inventory

By default, agent mode reads `cluster-inventory.yaml` from repo root.

Included default inventory:
- head: `192.168.8.121`
- worker: `192.168.8.138`
- RDMA ETH: `enp1s0f1np1`
- RDMA IB: `rocep1s0f1,roceP2p1s0f1`

## 2. Run Agent On Each Node

Build:

```bash
go build -o build/llama-swap-agent ./cmd/llama-swap-agent
```

Run:

```bash
LLAMA_SWAP_AGENT_LISTEN=:19090 ./build/llama-swap-agent
```

Optional auth:

```bash
export LLAMA_SWAP_AGENT_TOKEN_FILE=/path/to/agent.token
```

## 3. Run llama-swap On Control Host

```bash
export LLAMA_SWAP_CLUSTER_EXEC_MODE=agent
./build/llama-swap -config config.yaml
```

Optional:
- `LLAMA_SWAP_AGENT_TOKEN_FILE`: bearer token file used by control host when calling agents.
- `LLAMA_SWAP_CLUSTER_AGENT_REMOTE_ROOT`: repo root path on nodes (`$HOME/swap-laboratories` by default).
- `LLAMA_SWAP_CLUSTER_INVENTORY_FILE`: explicit inventory path override.
