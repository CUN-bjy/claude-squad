package tmux

import (
	cmd2 "claude-squad/cmd"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-squad/cmd/cmd_test"

	"github.com/stretchr/testify/require"
)

type MockPtyFactory struct {
	t *testing.T

	// Array of commands and the corresponding file handles representing PTYs.
	cmds  []*exec.Cmd
	files []*os.File
}

func (pt *MockPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	filePath := filepath.Join(pt.t.TempDir(), fmt.Sprintf("pty-%s-%d", pt.t.Name(), rand.Int31()))
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	if err == nil {
		pt.cmds = append(pt.cmds, cmd)
		pt.files = append(pt.files, f)
	}
	return f, err
}

func (pt *MockPtyFactory) Close() {}

func NewMockPtyFactory(t *testing.T) *MockPtyFactory {
	return &MockPtyFactory{
		t: t,
	}
}

// TestTrustScreenHandledAsync verifies that after the async fix:
// 1. Start() returns immediately (does NOT block 30s polling)
// 2. The background goroutine still detects the trust screen
// 3. The goroutine sends enter (0x0D) to the PTY
func TestTrustScreenHandledAsync(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	// Track when capture-pane is called — the trust prompt appears after
	// 300ms to simulate realistic startup latency.
	testStart := time.Now()
	created := false

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("no session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				if time.Since(testStart) < 300*time.Millisecond {
					return []byte("Loading claude..."), nil
				}
				return []byte("Do you trust the files in this folder?"), nil
			}
			return []byte(""), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession("trust-async", "claude", ptyFactory, cmdExec)

	// --- Verify Start() returns quickly ---
	start := time.Now()
	err := session.Start(workdir)
	startElapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, startElapsed, 1*time.Second,
		"Start() blocked — expected < 1s, got %v", startElapsed)
	t.Logf("Start() returned in %v", startElapsed)

	// --- Wait for the goroutine to detect trust screen and send enter ---
	// Trust screen appears at 300ms, goroutine polls every 100ms,
	// so it should be handled by ~500ms. Use generous timeout.
	time.Sleep(1 * time.Second)

	// ptmx is the second PTY file (set by Restore → attach-session).
	// TapEnter() writes 0x0D to it.
	require.GreaterOrEqual(t, len(ptyFactory.files), 2,
		"expected at least 2 PTY files (new-session + attach-session)")
	ptmx := ptyFactory.files[1]

	_, err = ptmx.Seek(0, 0)
	require.NoError(t, err)

	buf := make([]byte, 64)
	n, err := ptmx.Read(buf)
	require.NoError(t, err,
		"nothing written to PTY — goroutine never called TapEnter()")

	enterFound := false
	for i := 0; i < n; i++ {
		if buf[i] == 0x0D { // 0x0D = carriage return = enter
			enterFound = true
			break
		}
	}
	require.True(t, enterFound,
		"goroutine did not send enter keystroke (0x0D) to PTY; bytes written: %v", buf[:n])

	t.Logf("Trust screen handled: goroutine sent enter (0x0D) to PTY after trust prompt appeared")
}

// TestTrustScreenNoFalsePositive verifies that the goroutine does NOT send
// enter if the trust screen never appears (e.g. already trusted).
func TestTrustScreenNoFalsePositive(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("no session")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "capture-pane") {
				// Never show trust screen — just normal claude output
				return []byte("Claude is ready. Type your prompt."), nil
			}
			return []byte(""), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession("trust-nofp", "claude", ptyFactory, cmdExec)

	err := session.Start(workdir)
	require.NoError(t, err)

	// Wait enough time for the goroutine to have polled multiple times
	time.Sleep(500 * time.Millisecond)

	ptmx := ptyFactory.files[1]
	_, _ = ptmx.Seek(0, 0)

	buf := make([]byte, 64)
	n, _ := ptmx.Read(buf)

	for i := 0; i < n; i++ {
		require.NotEqual(t, byte(0x0D), buf[i],
			"goroutine sent enter even though trust screen was never shown")
	}

	t.Logf("Correctly did NOT send enter — trust screen was never shown")
}

func TestSanitizeName(t *testing.T) {
	session := NewTmuxSession("asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf", session.sanitizedName)

	session = NewTmuxSession("a sd f . . asdf", "program")
	require.Equal(t, TmuxPrefix+"asdf__asdf", session.sanitizedName)
}

func TestStartTmuxSession(t *testing.T) {
	ptyFactory := NewMockPtyFactory(t)

	created := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") && !created {
				created = true
				return fmt.Errorf("session already exists")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}

	workdir := t.TempDir()
	session := newTmuxSession("test-session", "claude", ptyFactory, cmdExec)

	err := session.Start(workdir)
	require.NoError(t, err)
	require.Equal(t, 2, len(ptyFactory.cmds))
	require.Equal(t, fmt.Sprintf("tmux new-session -d -s claudesquad_test-session -c %s claude", workdir),
		cmd2.ToString(ptyFactory.cmds[0]))
	require.Equal(t, "tmux attach-session -t claudesquad_test-session",
		cmd2.ToString(ptyFactory.cmds[1]))

	require.Equal(t, 2, len(ptyFactory.files))

	// File should be closed.
	_, err = ptyFactory.files[0].Stat()
	require.Error(t, err)
	// File should be open
	_, err = ptyFactory.files[1].Stat()
	require.NoError(t, err)
}
