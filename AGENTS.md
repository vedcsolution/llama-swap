## Project Description:

llama-swap is a light weight, transparent proxy server that provides automatic model swapping to llama.cpp's server.

## Tech stack

- golang
- typescript, vite and svelt5 for UI (located in ui/)

## Workflow Tasks

- when summarizing changes only include details that require further action
- just say "Done." when there is no further action
- use the github CLI `gh` to create pull requests and work with github
- Rules for creating pull requests:
  - keep them short and focused on changes.
  - never include a test plan
  - write the summary using the same style rules as commit message

## Testing

- Follow test naming conventions like `TestProxyManager_<test name>`, `TestProcessGroup_<test name>`, etc.
- Use `go test -v -run <name pattern for new tests>` to run any new tests you've written.
- Use `make test` for a quick proxy test pass (`go test -short -count=1 ./proxy/...`).
- Use `make test-dev` after running new tests for a quick over all test run. This runs `go test` and `staticcheck`. Fix any static checking errors. Use this only when changes are made to any code under the `proxy/` directory
- Use `make test-all` before completing work. This includes long running concurrency tests.

### Commit message example format:

```
proxy: add new feature

Add new feature that implements functionality X and Y.

- key change 1
- key change 2
- key change 3

fixes #123
```

## Code Reviews

- use three levels High, Medium, Low severity
- label each discovered issue with a label like H1, M2, L3 respectively
- High severity are must fix issues (security, race conditions, critical bugs)
- Medium severity are recommended improvements (coding style, missing functionality, inconsistencies)
- Low severity are nice to have changes and nits
- Include a suggestion with each discovered item
- Limit your code review to three items with the highest priority first
- Double check your discovered items and recommended remediations

## Build Image Policy

- Use only one Go build container image on this host: `golang:1.24-bookworm`.
- Do not use `golang:latest` or multiple Go tags in parallel.
- Preferred build command:
  - `docker run --rm -v $REPO_DIR:/src -w /src golang:1.24-bookworm go build -buildvcs=false -o build/llama-swap .`
- Cleanup policy for image hygiene:
  - Remove stale test-bench images (`tb__*`) and extra Go tags (any `golang:*` except `golang:1.24-bookworm`).
  - Keep runtime images required by recipes (`vllm-node:*`, `vllm-node-mxfp4:*`, `trtllm-node:*`, `llama-cpp-spark:*`).
- Systemd service must run fork binary from repo path, not global binary:
  - `ExecStart=$REPO_DIR/build/llama-swap ...`

## Restricciones De Edicion

- No se modifica `/home/csolutions_ai/swap-laboratories/backend/spark-vllm-docker`.
