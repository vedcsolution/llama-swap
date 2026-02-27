# Issue: chrome-devtools tool cannot run (missing Chrome binary)

Date: 2026-02-27

## Summary

The `mcp__chrome-devtools__new_page` tool cannot open the UI because Google Chrome is not installed at the expected path in this environment.

## Reproduction

Tool call:

```json
{
  "url": "http://192.168.8.127:18080/ui/#/cluster",
  "timeout": 120000
}
```

Returned error:

```text
Could not find Google Chrome executable for channel 'stable' at:
 - /Applications/Google Chrome.app/Contents/MacOS/Google Chrome.
```

## Impact

- Cannot perform UI validation via chrome-devtools tool.
- Cannot inspect browser-side network/runtime state from this environment.

## Requested resolution

- Install Google Chrome at the expected location, or configure the tool to a valid browser binary path.
- Re-run UI validation once tool is operational.
