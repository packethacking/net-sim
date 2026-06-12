package router

import (
	"errors"
	"log/slog"
	"syscall"
)

// rtNice is the niceness applied to sim-router and its TNC children when
// Options.RTPriority is set. The 10 ms pacing tickers and the children's
// software demodulators glitch under shared host load; a modest negative
// niceness keeps them ahead of batch work without the host-starvation
// risk of a real-time policy — deliberately plain niceness, not
// SCHED_FIFO.
const rtNice = -10

// setPriority is syscall.Setpriority(PRIO_PROCESS, ...), indirected so
// tests can stub it (raising priority needs CAP_SYS_NICE, which tests
// don't have and shouldn't need).
var setPriority = func(pid, nice int) error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, pid, nice)
}

// applyRTPriority renices one process (pid 0 = the calling process) to
// rtNice. Best-effort by design: without CAP_SYS_NICE the kernel returns
// EPERM/EACCES, which earns a single clear warning and the simulation
// carries on at normal priority.
func applyRTPriority(logger *slog.Logger, target string, pid int) {
	err := setPriority(pid, rtNice)
	if err == nil {
		return
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		logger.Warn("cannot raise scheduling priority — run with CAP_SYS_NICE (docker: --cap-add SYS_NICE / compose: cap_add: [SYS_NICE])",
			"target", target, "pid", pid, "nice", rtNice)
		return
	}
	logger.Warn("setpriority failed", "target", target, "pid", pid, "nice", rtNice, "err", err)
}
