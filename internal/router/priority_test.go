package router

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/packethacking/net-sim/internal/config"
)

// stubSetPriority replaces the setpriority syscall hook for the duration
// of a test, recording every call. Restores the real one on cleanup.
func stubSetPriority(t *testing.T, err error) *priorityCalls {
	t.Helper()
	calls := &priorityCalls{}
	orig := setPriority
	setPriority = func(pid, nice int) error {
		calls.mu.Lock()
		calls.pids = append(calls.pids, pid)
		calls.nices = append(calls.nices, nice)
		calls.mu.Unlock()
		return err
	}
	t.Cleanup(func() { setPriority = orig })
	return calls
}

type priorityCalls struct {
	mu    sync.Mutex
	pids  []int
	nices []int
}

func TestApplyRTPriorityInvokesSetpriority(t *testing.T) {
	calls := stubSetPriority(t, nil)
	applyRTPriority(quietLogger(), "a.vhf", 4242)
	if len(calls.pids) != 1 || calls.pids[0] != 4242 {
		t.Fatalf("setpriority pids = %v, want [4242]", calls.pids)
	}
	if calls.nices[0] != rtNice {
		t.Errorf("niceness = %d, want %d", calls.nices[0], rtNice)
	}
}

// TestApplyRTPriorityEPermWarnsAndContinues: without CAP_SYS_NICE the
// kernel returns EPERM; that must produce a single actionable warning
// (mentioning the capability) and no failure.
func TestApplyRTPriorityEPermWarnsAndContinues(t *testing.T) {
	stubSetPriority(t, syscall.EPERM)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	applyRTPriority(logger, "sim-router", 0) // must not panic / propagate
	out := buf.String()
	if !strings.Contains(out, "CAP_SYS_NICE") {
		t.Errorf("EPERM warning should tell the operator about CAP_SYS_NICE; got %q", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a WARN-level log line, got %q", out)
	}
}

// TestStartHonoursRTPriorityOption: the Options.RTPriority flag must
// reach the setpriority hook for the router's own process (pid 0) before
// children spawn. Children can't be started in a unit test (no TNC
// binary), so Start is allowed to fail afterwards — by then the self
// renice has either happened or the plumbing is broken.
func TestStartHonoursRTPriorityOption(t *testing.T) {
	calls := stubSetPriority(t, nil)
	cfg := &config.Config{
		TimeScale: 1,
		Nodes: []config.Node{{ID: "a", Ports: []config.Port{{
			ID:       "vhf",
			Modem:    config.Modem{Mode: config.ModeAFSK1200},
			KissPort: 18001,
		}}}},
	}
	_, err := Start(context.Background(), cfg, Options{
		RTPriority: true,
		SamoyedBin: "/nonexistent/samoyed-direwolf",
		WorkDir:    t.TempDir(),
		Logger:     quietLogger(),
	})
	if err == nil {
		t.Fatal("Start should fail without a TNC binary")
	}
	if len(calls.pids) != 1 || calls.pids[0] != 0 {
		t.Fatalf("setpriority pids = %v, want [0] (self renice before child spawn)", calls.pids)
	}
}

// TestStartSkipsPriorityWhenUnset: default behaviour leaves scheduling
// priority untouched.
func TestStartSkipsPriorityWhenUnset(t *testing.T) {
	calls := stubSetPriority(t, nil)
	cfg := &config.Config{
		TimeScale: 1,
		Nodes: []config.Node{{ID: "a", Ports: []config.Port{{
			ID:       "vhf",
			Modem:    config.Modem{Mode: config.ModeAFSK1200},
			KissPort: 18002,
		}}}},
	}
	_, err := Start(context.Background(), cfg, Options{
		SamoyedBin: "/nonexistent/samoyed-direwolf",
		WorkDir:    t.TempDir(),
		Logger:     quietLogger(),
	})
	if err == nil {
		t.Fatal("Start should fail without a TNC binary")
	}
	if len(calls.pids) != 0 {
		t.Fatalf("setpriority called %d times, want 0 when RTPriority unset", len(calls.pids))
	}
}

func TestApplyRTPriorityOtherErrorWarns(t *testing.T) {
	stubSetPriority(t, syscall.ESRCH)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	applyRTPriority(logger, "a.vhf", 99999)
	if !strings.Contains(buf.String(), "setpriority failed") {
		t.Errorf("expected a generic failure warning, got %q", buf.String())
	}
}
