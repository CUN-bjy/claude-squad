# Hypothesis 2: UpdateDiffStats() Runs Expensive Git Commands Synchronously

## Problem
`UpdateDiffStats()` in `session/instance.go:484-507` calls `gitWorktree.Diff()` which in `session/git/diff.go:25-51`:
1. Runs `git add -N .` to stage untracked files
2. Runs `git --no-pager diff <base-commit>` to compute the full diff
3. Parses the entire diff output line by line

For large repositories, this can take 1-3 seconds per call.

## Affected Code
- `session/instance.go:484-507` — UpdateDiffStats()
- `session/git/diff.go:25-51` — Diff()
- `session/git/diff.go:29` — `git add -N .`
- `session/git/diff.go:35` — `git --no-pager diff`

## Proposed Fix
1. Add a mutex and `diffUpdating` flag to `Instance`
2. `UpdateDiffStats()` checks the flag; if already updating, returns immediately (uses cached value)
3. Spawns a goroutine to compute the diff
4. Goroutine stores result in `diffStats` field (protected by mutex) and clears the flag
5. `GetDiffStats()` reads from the cache (protected by mutex)

## Expected Impact
MEDIUM-HIGH — Eliminates git subprocess blocking from the event loop. Diff stats will be slightly stale (updated every ~500ms asynchronously) but this is invisible to users.
