# Ticket: MCP chrome-devtools unavailable (missing Chrome executable)

Date: 2026-02-25
Reporter: Codex agent
Status: Open

## Summary
The `mcp__chrome-devtools` tool fails immediately and cannot open browser pages for UI diagnostics.

## Error
`Could not find Google Chrome executable for channel stable at:`
`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome.`

## Impact
- UI bug triage that requires DOM/runtime inspection is blocked.
- Cannot validate button disabled state/click handlers from the running UI.

## Reproduction
1. Call `mcp__chrome-devtools__new_page` with URL `http://192.168.8.121:8080/ui/#/models`.
2. Tool returns missing Chrome executable error.

## Requested Fix
- Install/configure a Chrome executable compatible with the MCP chrome-devtools runtime.
- Or configure MCP to use an existing browser binary path.

## Notes
Attempted to create a GitHub issue via `gh issue create -R vedcsolution/swap-laboratories`, but repository issues are disabled.
