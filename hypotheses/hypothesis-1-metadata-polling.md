# Hypothesis 1: tickUpdateMetadataMessage Blocks Event Loop

## Problem
The `tickUpdateMetadataMessage` handler in `app/app.go:203-222` iterates over ALL running instances and synchronously calls:
- `instance.HasUpdated()` → calls `tmux.HasUpdated()` → calls `CapturePaneContent()` which runs `tmux capture-pane` subprocess
- `instance.UpdateDiffStats()` → calls `gitWorktree.Diff()` which runs `git add -N .` + `git --no-pager diff` subprocesses

This all executes in the bubbletea `Update()` method, blocking ALL UI event processing.

## Affected Code
- `app/app.go:203-222` — tickUpdateMetadataMessage case in Update()
- `session/instance.go:304-309` — HasUpdated() wrapper
- `session/instance.go:484-507` — UpdateDiffStats()
- `session/tmux/tmux.go:246-267` — HasUpdated() with CapturePaneContent()
- `session/git/diff.go:25-51` — Diff() running git commands

## Proposed Fix
Convert the metadata update to an async pattern:
1. Create a new message type `metadataResultMsg` to carry results
2. When `tickUpdateMetadataMessage` is received, return a `tea.Cmd` that runs the expensive work in a goroutine
3. The goroutine collects results for all instances and sends back `metadataResultMsg`
4. The `metadataResultMsg` handler applies the results without blocking

## Expected Impact
HIGH — This is the primary cause of multi-second keystroke delays. Each tick currently blocks the event loop for the time it takes to run tmux capture-pane + git add + git diff for EVERY active instance.
