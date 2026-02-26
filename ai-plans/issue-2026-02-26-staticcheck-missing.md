# Issue: staticcheck tool missing in local environment

Date: 2026-02-26

## Summary

`staticcheck` is not available in the current shell environment, so static analysis cannot be executed as required by `make test-dev`.

## Reproduction

```bash
staticcheck ./proxy/... ./cmd/llama-swap-agent/...
```

Result:

```text
zsh:1: command not found: staticcheck
```

## Impact

- Unable to complete static analysis gate locally.
- `make test-dev` cannot run fully to completion when staticcheck is expected.

## Requested resolution

- Install or expose `staticcheck` in PATH for the development environment.
- Keep version aligned with Go `1.24.x` toolchain used by this repo.
