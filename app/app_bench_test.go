package app

import (
	"claude-squad/cmd/cmd_test"
	"claude-squad/config"
	"claude-squad/log"
	"claude-squad/session"
	"claude-squad/session/git"
	"claude-squad/session/tmux"
	"claude-squad/ui"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// mockPtyFactory creates fake PTY files for testing without real tmux.
// It delegates commands to cmdExec so that session-creation flags are set.
type mockPtyFactory struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
}

func (pt *mockPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	f, err := os.CreateTemp(pt.t.TempDir(), "pty-*")
	if err == nil {
		// Relay the command so the mock can track state (e.g. sessionCreated).
		_ = pt.cmdExec.Run(cmd)
	}
	return f, err
}
func (pt *mockPtyFactory) Close() {}

// newSlowCmdExec returns a MockCmdExec whose Output() sleeps for the given
// duration before returning, simulating a slow subprocess (e.g. tmux
// capture-pane, git diff).
func newSlowCmdExec(capturePaneDelay, gitDiffDelay time.Duration) cmd_test.MockCmdExec {
	createdSessions := map[string]bool{}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := strings.Join(cmd.Args, " ")
			if strings.Contains(s, "has-session") {
				// Extract session name from "-t=claudesquad_..."
				for _, arg := range cmd.Args {
					if strings.HasPrefix(arg, "-t=") {
						name := strings.TrimPrefix(arg, "-t=")
						if createdSessions[name] {
							return nil
						}
						return fmt.Errorf("no session: %s", name)
					}
				}
				return fmt.Errorf("no session")
			}
			if strings.Contains(s, "new-session") {
				// Extract session name from "-s <name>"
				for i, arg := range cmd.Args {
					if arg == "-s" && i+1 < len(cmd.Args) {
						createdSessions[cmd.Args[i+1]] = true
						break
					}
				}
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := strings.Join(cmd.Args, " ")
			if strings.Contains(s, "capture-pane") {
				time.Sleep(capturePaneDelay)
				return []byte("mock pane content"), nil
			}
			if strings.Contains(s, "diff") {
				time.Sleep(gitDiffDelay)
				return []byte("+added line\n-removed line\n"), nil
			}
			if strings.Contains(s, "add -N") {
				time.Sleep(gitDiffDelay / 2)
				return nil, nil
			}
			return []byte(""), nil
		},
	}
}

// setupGitRepo initializes a minimal git repo for worktree tests.
func setupGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "--local", "user.email", "test@test.com"},
		{"config", "--local", "user.name", "Test"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		require.NoError(t, c.Run())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0644))
	c := exec.Command("git", "add", ".")
	c.Dir = dir
	require.NoError(t, c.Run())
	c = exec.Command("git", "commit", "-m", "init")
	c.Dir = dir
	require.NoError(t, c.Run())
}

// createTestInstance creates a started Instance backed by mocks with the given
// subprocess delays.
func createTestInstance(t *testing.T, name string, cmdExec cmd_test.MockCmdExec) *session.Instance {
	t.Helper()
	workdir := t.TempDir()
	setupGitRepo(t, workdir)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   name,
		Path:    workdir,
		Program: "bash", // not "claude" so Start() skips trust‑screen polling
	})
	require.NoError(t, err)

	pty := &mockPtyFactory{t: t, cmdExec: cmdExec}
	tmuxSess := tmux.NewTmuxSessionWithDeps(name, "bash", pty, cmdExec)
	inst.SetTmuxSession(tmuxSess)
	require.NoError(t, inst.Start(true))
	return inst
}

// buildHome creates a minimal *home suitable for calling Update().
func buildHome(t *testing.T, instances []*session.Instance) *home {
	t.Helper()
	s := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	list := ui.NewList(&s, false)
	for _, inst := range instances {
		list.AddInstance(inst)()
	}
	if len(instances) > 0 {
		list.SetSelectedInstance(0)
	}
	return &home{
		ctx:          context.Background(),
		state:        stateDefault,
		appConfig:    config.DefaultConfig(),
		list:         list,
		menu:         ui.NewMenu(),
		tabbedWindow: ui.NewTabbedWindow(ui.NewPreviewPane(), ui.NewDiffPane()),
		errBox:       ui.NewErrBox(),
		spinner:      s,
	}
}

// ---------------------------------------------------------------------------
// "Before" simulation — reproduce the OLD synchronous behavior
// ---------------------------------------------------------------------------

// simulateOldMetadataTick reproduces what the old tickUpdateMetadataMessage
// handler did: iterate over all instances and call HasUpdated() + Diff()
// synchronously, blocking the caller for the full duration.
func simulateOldMetadataTick(h *home) {
	for _, instance := range h.list.GetInstances() {
		if !instance.Started() || instance.Paused() {
			continue
		}
		// These calls block — each spawns subprocess(es).
		instance.HasUpdated()
		instance.UpdateDiffStats()
	}
}

// simulateOldPreviewTick reproduces the old previewTickMsg handler:
// synchronously captures pane content via instanceChanged().
func simulateOldPreviewTick(h *home) {
	h.instanceChanged()
}

// ---------------------------------------------------------------------------
// Benchmarks — measure Update() return time for critical messages
// ---------------------------------------------------------------------------

// BenchmarkUpdateMetadataTick measures how fast Update() returns when it
// receives a tickUpdateMetadataMessage. Before the async fix this blocked for
// the duration of every subprocess call; after the fix it returns immediately,
// delegating the work to a tea.Cmd goroutine.
func BenchmarkUpdateMetadataTick(b *testing.B) {
	log.Initialize(false)
	defer log.Close()

	for _, numInstances := range []int{1, 3, 5} {
		for _, delay := range []time.Duration{
			10 * time.Millisecond,
			50 * time.Millisecond,
		} {
			name := fmt.Sprintf("instances=%d/delay=%v", numInstances, delay)
			b.Run(name, func(b *testing.B) {
				cmdExec := newSlowCmdExec(delay, delay)
				var instances []*session.Instance
				for i := range numInstances {
					inst := createTestInstance(
						&testing.T{}, // benchmarks can't use b as *testing.T
						fmt.Sprintf("bench-%d-%d", numInstances, i),
						cmdExec,
					)
					instances = append(instances, inst)
				}
				h := buildHome(&testing.T{}, instances)

				msg := tickUpdateMetadataMessage{}
				b.ResetTimer()
				for range b.N {
					_, cmd := h.Update(msg)
					// cmd is the goroutine — we DON'T execute it here on
					// purpose. The whole point is that Update() itself should
					// return near‑instantly.
					_ = cmd
				}
			})
		}
	}
}

// BenchmarkUpdatePreviewTick is the same idea for the preview refresh tick.
func BenchmarkUpdatePreviewTick(b *testing.B) {
	log.Initialize(false)
	defer log.Close()

	for _, delay := range []time.Duration{10 * time.Millisecond, 50 * time.Millisecond} {
		name := fmt.Sprintf("delay=%v", delay)
		b.Run(name, func(b *testing.B) {
			cmdExec := newSlowCmdExec(delay, delay)
			inst := createTestInstance(
				&testing.T{},
				"bench-preview",
				cmdExec,
			)
			h := buildHome(&testing.T{}, []*session.Instance{inst})

			msg := previewTickMsg{}
			b.ResetTimer()
			for range b.N {
				_, cmd := h.Update(msg)
				_ = cmd
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests — verify the async contract quantitatively
// ---------------------------------------------------------------------------

// TestMetadataTickReturnsWithinBudget verifies that Update() for a metadata
// tick returns within 5 ms even when the underlying subprocesses each take
// 100 ms.  With 3 instances and 2 subprocess calls per instance (capture-pane
// + git diff), the OLD synchronous code would block for ≈ 600 ms.
func TestMetadataTickReturnsWithinBudget(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const subprocessDelay = 100 * time.Millisecond
	const numInstances = 3
	const budget = 5 * time.Millisecond // Update() must return within this

	cmdExec := newSlowCmdExec(subprocessDelay, subprocessDelay)
	var instances []*session.Instance
	for i := range numInstances {
		inst := createTestInstance(t, fmt.Sprintf("meta-budget-%d", i), cmdExec)
		instances = append(instances, inst)
	}
	h := buildHome(t, instances)

	start := time.Now()
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	elapsed := time.Since(start)

	t.Logf("Update(tickUpdateMetadataMessage) returned in %v (budget %v)", elapsed, budget)
	require.Less(t, elapsed, budget,
		"Update() blocked the event loop — expected < %v, got %v (old sync path would take ≈%v)",
		budget, elapsed, subprocessDelay*time.Duration(numInstances)*2)

	// The returned Cmd should be non‑nil (the goroutine).
	require.NotNil(t, cmd, "expected a tea.Cmd to be returned for async work")
}

// TestPreviewTickReturnsWithinBudget is the same check for the preview tick.
func TestPreviewTickReturnsWithinBudget(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const subprocessDelay = 100 * time.Millisecond
	const budget = 5 * time.Millisecond

	cmdExec := newSlowCmdExec(subprocessDelay, subprocessDelay)
	inst := createTestInstance(t, "preview-budget", cmdExec)
	h := buildHome(t, []*session.Instance{inst})

	start := time.Now()
	_, cmd := h.Update(previewTickMsg{})
	elapsed := time.Since(start)

	t.Logf("Update(previewTickMsg) returned in %v (budget %v)", elapsed, budget)
	require.Less(t, elapsed, budget,
		"Update() blocked the event loop — expected < %v, got %v (old sync path would take ≈%v)",
		budget, elapsed, subprocessDelay)

	require.NotNil(t, cmd, "expected a tea.Cmd to be returned for async work")
}

// TestMetadataAsyncResultAppliesCorrectly verifies the full round‑trip:
// Update() returns a Cmd, executing that Cmd produces a metadataResultMsg,
// and handling that message correctly updates instance state.
func TestMetadataAsyncResultAppliesCorrectly(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// Use zero delay so the test is fast.
	cmdExec := newSlowCmdExec(0, 0)
	inst := createTestInstance(t, "meta-roundtrip", cmdExec)
	h := buildHome(t, []*session.Instance{inst})

	// Step 1: send the tick, get back the async Cmd.
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	require.NotNil(t, cmd)

	// Step 2: execute the Cmd (simulates bubbletea running it in a goroutine).
	msg := cmd()
	resultMsg, ok := msg.(metadataResultMsg)
	require.True(t, ok, "expected metadataResultMsg, got %T", msg)
	require.Len(t, resultMsg.results, 1)

	// Step 3: feed the result back into Update().
	_, cmd2 := h.Update(resultMsg)

	// The handler should schedule the next tick (tickUpdateMetadataCmd).
	require.NotNil(t, cmd2, "expected tickUpdateMetadataCmd to be scheduled")

	// Verify state was applied — instance should be Running or Ready.
	status := inst.Status
	require.True(t, status == session.Running || status == session.Ready,
		"expected Running or Ready, got %v", status)
}

// TestPreviewAsyncResultAppliesCorrectly verifies the preview round‑trip.
func TestPreviewAsyncResultAppliesCorrectly(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	cmdExec := newSlowCmdExec(0, 0)
	inst := createTestInstance(t, "preview-roundtrip", cmdExec)
	h := buildHome(t, []*session.Instance{inst})

	// Step 1: send the tick.
	_, cmd := h.Update(previewTickMsg{})
	require.NotNil(t, cmd)

	// Step 2: execute the Cmd.
	msg := cmd()
	resultMsg, ok := msg.(previewResultMsg)
	require.True(t, ok, "expected previewResultMsg, got %T", msg)
	require.NoError(t, resultMsg.err)
	require.Equal(t, inst, resultMsg.instance)

	// Step 3: feed the result back into Update().
	_, cmd2 := h.Update(resultMsg)
	require.NotNil(t, cmd2, "expected next preview tick to be scheduled")
}

// TestTrustScreenDoesNotBlockStart verifies that tmux.Start() returns
// quickly even for a "claude" program (which triggers trust‑screen polling).
// Before the fix, Start() would block for up to 30s.
func TestTrustScreenDoesNotBlockStart(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const budget = 2 * time.Second

	sessionCreated := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := strings.Join(cmd.Args, " ")
			if strings.Contains(s, "has-session") {
				if sessionCreated {
					return nil
				}
				return fmt.Errorf("no session")
			}
			if strings.Contains(s, "new-session") {
				sessionCreated = true
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := strings.Join(cmd.Args, " ")
			if strings.Contains(s, "capture-pane") {
				// Simulate slow startup — never return the trust prompt so the
				// goroutine keeps polling (but Start should already have
				// returned).
				time.Sleep(200 * time.Millisecond)
				return []byte("loading..."), nil
			}
			return []byte(""), nil
		},
	}

	workdir := t.TempDir()
	setupGitRepo(t, workdir)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "trust-test",
		Path:    workdir,
		Program: "claude", // triggers trust screen path
	})
	require.NoError(t, err)

	pty := &mockPtyFactory{t: t, cmdExec: cmdExec}
	tmuxSess := tmux.NewTmuxSessionWithDeps("trust-test", "claude", pty, cmdExec)
	inst.SetTmuxSession(tmuxSess)

	start := time.Now()
	err = inst.Start(true)
	elapsed := time.Since(start)

	t.Logf("Start() returned in %v (budget %v)", elapsed, budget)
	require.NoError(t, err)
	require.Less(t, elapsed, budget,
		"Start() blocked on trust screen polling — expected < %v, got %v", budget, elapsed)
}

// TestEndToEndEventLoopResponsiveness simulates a realistic scenario: 3
// sessions with 50 ms subprocess delays, and measures that the event loop
// can process a key press within a strict time budget while metadata &
// preview ticks are also being handled.
func TestEndToEndEventLoopResponsiveness(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const subprocessDelay = 50 * time.Millisecond
	const numInstances = 3
	const budget = 10 * time.Millisecond

	cmdExec := newSlowCmdExec(subprocessDelay, subprocessDelay)

	var instances []*session.Instance
	for i := range numInstances {
		inst := createTestInstance(t, fmt.Sprintf("e2e-%d", i), cmdExec)
		instances = append(instances, inst)
	}
	h := buildHome(t, instances)

	// Fire a metadata tick (returns async cmd — does NOT block).
	_, _ = h.Update(tickUpdateMetadataMessage{})

	// Fire a preview tick (returns async cmd — does NOT block).
	_, _ = h.Update(previewTickMsg{})

	// Now simulate a user key press and verify it returns instantly.
	// 'j' = KeyDown in the default keymap.
	keyMsg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}

	start := time.Now()
	_, _ = h.Update(keyMsg)
	elapsed := time.Since(start)

	t.Logf("Key press processed in %v (budget %v)", elapsed, budget)
	require.Less(t, elapsed, budget,
		"Key press was delayed — expected < %v, got %v", budget, elapsed)
}

// TestDiffStatsSetterUsedByAsyncPath verifies that SetDiffStats() correctly
// stores and retrieves diff stats (used by the async metadata path).
func TestDiffStatsSetterUsedByAsyncPath(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	cmdExec := newSlowCmdExec(0, 0)
	inst := createTestInstance(t, "diffstats-setter", cmdExec)

	// Initially nil.
	require.Nil(t, inst.GetDiffStats())

	// Set via the new setter.
	stats := &git.DiffStats{Added: 10, Removed: 3, Content: "+foo\n-bar"}
	inst.SetDiffStats(stats)

	got := inst.GetDiffStats()
	require.NotNil(t, got)
	require.Equal(t, 10, got.Added)
	require.Equal(t, 3, got.Removed)

	// Setting nil clears it.
	inst.SetDiffStats(nil)
	require.Nil(t, inst.GetDiffStats())
}

// ---------------------------------------------------------------------------
// Before vs After comparison — directly measure the difference
// ---------------------------------------------------------------------------

// TestBeforeAfterMetadataTick measures the OLD synchronous path vs the NEW
// async path side-by-side and prints both timings for comparison.
func TestBeforeAfterMetadataTick(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const subprocessDelay = 50 * time.Millisecond
	const numInstances = 3
	// Old path: 3 instances × (capture-pane 50ms + git-add 25ms + git-diff 50ms) ≈ 375ms
	// New path: < 1ms (just builds closure and returns)

	cmdExec := newSlowCmdExec(subprocessDelay, subprocessDelay)
	var instances []*session.Instance
	for i := range numInstances {
		inst := createTestInstance(t, fmt.Sprintf("cmp-meta-%d", i), cmdExec)
		instances = append(instances, inst)
	}
	h := buildHome(t, instances)

	// --- OLD (synchronous) path ---
	startOld := time.Now()
	simulateOldMetadataTick(h)
	elapsedOld := time.Since(startOld)

	// --- NEW (async) path ---
	startNew := time.Now()
	_, _ = h.Update(tickUpdateMetadataMessage{})
	elapsedNew := time.Since(startNew)

	speedup := float64(elapsedOld) / float64(elapsedNew)

	t.Logf("=== Metadata tick: %d instances, %v subprocess delay ===", numInstances, subprocessDelay)
	t.Logf("  OLD (sync):  %v", elapsedOld)
	t.Logf("  NEW (async): %v", elapsedNew)
	t.Logf("  Speedup:     %.0fx", speedup)

	require.Greater(t, elapsedOld, 100*time.Millisecond,
		"OLD path should block for at least 100ms with %v delay", subprocessDelay)
	require.Less(t, elapsedNew, 5*time.Millisecond,
		"NEW path should return in < 5ms")
}

// TestBeforeAfterPreviewTick measures the OLD synchronous preview capture vs
// the NEW async path.
func TestBeforeAfterPreviewTick(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const subprocessDelay = 50 * time.Millisecond
	// Old path: capture-pane 50ms (blocking in Update)
	// New path: < 1ms (returns closure)

	cmdExec := newSlowCmdExec(subprocessDelay, subprocessDelay)
	inst := createTestInstance(t, "cmp-preview", cmdExec)
	h := buildHome(t, []*session.Instance{inst})

	// --- OLD (synchronous) path ---
	startOld := time.Now()
	simulateOldPreviewTick(h)
	elapsedOld := time.Since(startOld)

	// --- NEW (async) path ---
	startNew := time.Now()
	_, _ = h.Update(previewTickMsg{})
	elapsedNew := time.Since(startNew)

	speedup := float64(elapsedOld) / float64(elapsedNew)

	t.Logf("=== Preview tick: 1 instance, %v subprocess delay ===", subprocessDelay)
	t.Logf("  OLD (sync):  %v", elapsedOld)
	t.Logf("  NEW (async): %v", elapsedNew)
	t.Logf("  Speedup:     %.0fx", speedup)

	require.Greater(t, elapsedOld, 30*time.Millisecond,
		"OLD path should block for at least 30ms with %v delay", subprocessDelay)
	require.Less(t, elapsedNew, 5*time.Millisecond,
		"NEW path should return in < 5ms")
}

// TestBeforeAfterEndToEnd measures the FULL round-trip: Update() dispatches
// async work → goroutine executes subprocess calls → result message comes
// back → Update() applies state. This shows:
// - OLD: UI blocked for entire duration (user can't do anything)
// - NEW: UI blocked only for dispatch + apply (~µs), subprocess runs in background
func TestBeforeAfterEndToEnd(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const captureDelay = 100 * time.Millisecond
	const diffDelay = 200 * time.Millisecond
	const numInstances = 3

	cmdExec := newSlowCmdExec(captureDelay, diffDelay)
	var instances []*session.Instance
	for i := range numInstances {
		inst := createTestInstance(t, fmt.Sprintf("e2e-full-%d", i), cmdExec)
		instances = append(instances, inst)
	}
	h := buildHome(t, instances)

	t.Logf("=== End-to-End: %d instances, capture=%v, diff=%v ===", numInstances, captureDelay, diffDelay)
	t.Logf("")

	// --- OLD: everything blocks the UI thread ---
	startOld := time.Now()
	simulateOldMetadataTick(h)
	totalOld := time.Since(startOld)
	t.Logf("OLD path (sync, all on UI thread):")
	t.Logf("  Total time:       %v", totalOld)
	t.Logf("  UI blocked for:   %v  ← user sees freeze", totalOld)
	t.Logf("")

	// --- NEW: dispatch is instant, work runs in goroutine ---
	// Phase 1: Update() returns immediately
	startDispatch := time.Now()
	_, cmd := h.Update(tickUpdateMetadataMessage{})
	dispatchTime := time.Since(startDispatch)

	// Phase 2: goroutine executes (simulates bubbletea running tea.Cmd)
	startWork := time.Now()
	resultMsg := cmd() // this blocks for subprocess delays
	workTime := time.Since(startWork)

	// Phase 3: result applied on UI thread
	startApply := time.Now()
	_, _ = h.Update(resultMsg)
	applyTime := time.Since(startApply)

	totalNew := dispatchTime + workTime + applyTime
	uiBlocked := dispatchTime + applyTime // only these block the UI

	t.Logf("NEW path (async):")
	t.Logf("  Phase 1 - dispatch (UI thread): %v", dispatchTime)
	t.Logf("  Phase 2 - subprocess (goroutine): %v  ← runs in background", workTime)
	t.Logf("  Phase 3 - apply result (UI thread): %v", applyTime)
	t.Logf("  Total wall time:  %v", totalNew)
	t.Logf("  UI blocked for:   %v  ← user sees no freeze", uiBlocked)
	t.Logf("")
	t.Logf("Comparison:")
	t.Logf("  Total time:  OLD %v vs NEW %v (similar — same work is done)", totalOld, totalNew)
	t.Logf("  UI blocked:  OLD %v vs NEW %v (%.0fx improvement)", totalOld, uiBlocked, float64(totalOld)/float64(uiBlocked))

	// The total wall time should be similar (same subprocess work).
	// But UI blocking time should be drastically different.
	require.Less(t, uiBlocked, 5*time.Millisecond,
		"UI should be blocked for < 5ms, got %v", uiBlocked)
	require.Greater(t, totalOld, 100*time.Millisecond,
		"OLD path should block UI for > 100ms")
}

// TestBeforeAfterScalingWithInstances shows how the OLD path degrades linearly
// with instance count while the NEW path stays constant.
// Tests with both conservative and realistic subprocess delays.
func TestBeforeAfterScalingWithInstances(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// Test with multiple delay profiles:
	// - 30ms: best-case (fast SSD, small repo)
	// - 200ms: typical (medium repo, normal system load)
	// - 500ms: slow (large repo, HDD, or high system load)
	delays := []struct {
		name           string
		capturePane    time.Duration
		gitDiff        time.Duration
	}{
		{"fast-ssd", 30 * time.Millisecond, 30 * time.Millisecond},
		{"typical", 100 * time.Millisecond, 200 * time.Millisecond},
		{"slow-system", 200 * time.Millisecond, 500 * time.Millisecond},
	}

	for _, d := range delays {
		t.Logf("")
		t.Logf("=== %s (capture-pane=%v, git-diff=%v) ===", d.name, d.capturePane, d.gitDiff)
		t.Logf("%-12s %15s %15s %10s", "Instances", "OLD (sync)", "NEW (async)", "Speedup")
		t.Logf("%-12s %15s %15s %10s", "─────────", "──────────", "──────────", "───────")

		for _, n := range []int{1, 3, 5, 8} {
			cmdExec := newSlowCmdExec(d.capturePane, d.gitDiff)
			var instances []*session.Instance
			for i := range n {
				inst := createTestInstance(t, fmt.Sprintf("scale-%s-%d-%d", d.name, n, i), cmdExec)
				instances = append(instances, inst)
			}
			h := buildHome(t, instances)

			// OLD
			startOld := time.Now()
			simulateOldMetadataTick(h)
			elapsedOld := time.Since(startOld)

			// NEW
			startNew := time.Now()
			_, _ = h.Update(tickUpdateMetadataMessage{})
			elapsedNew := time.Since(startNew)

			speedup := float64(elapsedOld) / float64(elapsedNew)
			t.Logf("%-12d %15v %15v %9.0fx", n, elapsedOld, elapsedNew, speedup)
		}
	}
}
