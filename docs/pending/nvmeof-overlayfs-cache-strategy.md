# Pending: NVMe-oF cache strategy without backend code changes

Status: pending  
Scope: `spark-vllm-docker`, `spark-trtllm-docker`, `spark-sqlang-docker`  
Goal: reduce duplicated model/cache footprint across cluster nodes while keeping current backend paths unchanged.

## Context

Current backends tend to use local `./cache` paths. Moving directly to a shared NVMe-oF path is hard when upstream backends do not expose configurable cache layouts for every component.

We need a path-preserving approach that does not require patching backend repositories.

## Constraint

Keep existing paths intact:

- `~/spark-vllm-docker/cache`
- `~/spark-trtllm-docker/cache`
- `~/spark-sqlang-docker/cache`
- custom backend path cache layout

## Proposed approach

Use system-level redirection under the same visible `./cache` path:

1. `bind mount` (simple baseline), or
2. `overlayfs` (recommended)

Recommended `overlayfs` model:

- `lowerdir`: shared NVMe-oF cache (read-mostly base artifacts)
- `upperdir`: per-node local writable layer
- `workdir`: per-node local overlay workdir
- `mountpoint`: backend `./cache`

This keeps backend behavior unchanged while allowing shared reads and local writes/locks.

## Data placement policy

Share (NVMe-oF lowerdir):

- model weights and blobs
- read-mostly artifacts

Keep local (upperdir or explicit local dirs):

- lock-heavy datasets/indexes
- temporary/build caches
- tool-specific ephemeral files

## Rollout plan (canary)

1. Apply to one backend on one node.
2. Validate startup, model load, and benchmark behavior.
3. Monitor reconnect/latency/error counters.
4. Expand to second node.
5. Repeat for remaining backends.

## Success criteria

- No backend path changes required.
- Lower duplicated disk usage across nodes.
- No increase in lock/contention errors.
- Stable throughput/latency during sustained benchmarks.

## Notes

- This is intentionally backend-maintainer-independent.
- It complements the Cluster storage baseline view, which already exposes duplicated paths and helps track progress.
