# Hypothesis 4: Trust Screen Polling Blocks Session Start

## Problem
In `session/tmux/tmux.go:151-189`, when starting a session with Claude/Aider/Gemini, the code polls for a "trust screen" in a synchronous loop:
- Loop runs for up to 30 seconds (Claude) or 45 seconds (Aider/Gemini)
- Each iteration calls `CapturePaneContent()` (subprocess)
- Uses exponential backoff starting at 100ms, capped at 1 second
- This blocks the `Start()` call, which blocks session creation entirely

## Affected Code
- `session/tmux/tmux.go:152-189` — Trust screen polling loop
- `session/tmux/tmux.go:171` — CapturePaneContent() called in loop
- `session/instance.go:239` — Start() calls tmuxSession.Start()

## Proposed Fix
Run the trust screen polling in a goroutine so `Start()` returns immediately:
1. After the tmux session is created and restored, spawn a goroutine for trust screen handling
2. The goroutine polls and handles the trust screen asynchronously
3. Start() returns immediately, allowing the UI to remain responsive

## Expected Impact
MEDIUM — Only affects session creation, but eliminates the 10-30 second blocking delay when creating new sessions. The trust screen will still be handled, just asynchronously.
