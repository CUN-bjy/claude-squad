# Hypothesis 3: Preview Pane Capture Blocks Every 100ms

## Problem
In `app/app.go:191-199`, `previewTickMsg` fires every 100ms. The handler calls `instanceChanged()` which calls `tabbedWindow.UpdatePreview()` → `PreviewPane.UpdateContent()` → `instance.Preview()` → `tmuxSession.CapturePaneContent()`.

`CapturePaneContent()` in `session/tmux/tmux.go:476-484` runs `tmux capture-pane -p -e -J` as a subprocess. This blocks the event loop every 100ms.

## Affected Code
- `app/app.go:191-199` — previewTickMsg handler
- `app/app.go:636-650` — instanceChanged()
- `ui/preview.go:53-113` — UpdateContent()
- `session/instance.go:297-302` — Preview()
- `session/tmux/tmux.go:476-484` — CapturePaneContent()

## Proposed Fix
1. The `previewTickMsg` handler should return a `tea.Cmd` that captures content in a goroutine
2. Create a new message type `previewResultMsg` with the captured content
3. Handle `previewResultMsg` in Update() to apply the content without blocking
4. Increase tick interval from 100ms to 200ms for reduced overhead

## Expected Impact
HIGH — This runs 10x per second and each call spawns a subprocess. Making it async eliminates a major source of event loop blocking.
