// sim-router is the entrypoint for the AX.25 packet network simulator.
//
//	sim-router -config configs/hidden-node.yaml [-v] [-samoyed PATH]
//
// Reads a YAML topology, spawns one samoyed-direwolf child per port,
// routes audio between ports per the topology with FM capture-effect
// mixing, and exposes each port's KISS interface on the configured TCP
// port.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/router"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML topology file (required)")
	verbose := flag.Bool("v", false, "verbose / debug logging")
	samoyedPath := flag.String("samoyed", "", "path to samoyed-direwolf (default: search $PATH and common install paths)")
	direwolfPath := flag.String("direwolf", "", "path to direwolf (default: search $PATH)")
	workDir := flag.String("workdir", "", "scratch dir for per-port config files / FIFOs (default: a unique subdir of $TMPDIR)")
	recordDir := flag.String("record", "", "if set, record all per-port TX and RX audio to a timestamped subdirectory of this path")
	composite := flag.String("composite", "", "comma-separated transmitter ports (e.g. a.vhf,b.vhf) to composite into one real-time, sample-aligned WAV (one TX per channel — stereo for two). Requires -record for the output dir")
	timeScale := flag.Float64("time-scale", 0, "run the simulation N x faster than wall clock (>= 1.0; overrides the config's time_scale; see README for the fidelity caveat)")
	rtPriority := flag.Bool("rt-priority", false, "renice sim-router and every TNC child to -10 for smoother audio pacing under host load (best-effort; needs CAP_SYS_NICE)")
	flag.Parse()

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "sim-router: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	if *composite != "" && *recordDir == "" {
		fmt.Fprintln(os.Stderr, "sim-router: -composite requires -record DIR (for the output directory)")
		os.Exit(2)
	}
	compositePorts, err := parseCompositePorts(*composite)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sim-router:", err)
		os.Exit(2)
	}

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}
	if *timeScale != 0 {
		if *timeScale < 1 {
			logger.Error("-time-scale must be >= 1.0 (slower-than-real-time is not supported)", "got", *timeScale)
			os.Exit(2)
		}
		cfg.TimeScale = *timeScale
	}
	if cfg.TimeScale > 1 {
		// Accelerated-testing mode, not a calibrated CSMA simulation: the
		// TNC children's wall-clock waits (persist/slottime, timeouts) do
		// NOT scale. See README "time_scale".
		logger.Warn("time_scale active — TNC CSMA timing does not scale; hosts must scale their own protocol timers", "time_scale", cfg.TimeScale)
	}

	samoyedBin, samoyedErr := resolveSamoyed(*samoyedPath)
	direwolfBin, direwolfErr := resolveDirewolf(*direwolfPath)
	// Each backend is only required if at least one port uses it; defer
	// the strict check to router.Start, but warn now so the operator
	// notices missing binaries before sending KISS frames at a port that
	// can't actually start.
	if samoyedErr != nil {
		logger.Warn("samoyed-direwolf not found; ports with tnc=samoyed (the default) won't start", "err", samoyedErr)
	}
	if direwolfErr != nil {
		logger.Warn("direwolf not found; ports with tnc=direwolf won't start", "err", direwolfErr)
	}

	wd, err := resolveWorkDir(*workDir)
	if err != nil {
		logger.Error("workdir", "err", err)
		os.Exit(1)
	}

	logger.Info("starting", "config", *cfgPath, "samoyed", samoyedBin, "direwolf", direwolfBin, "workdir", wd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := router.Start(ctx, cfg, router.Options{
		SamoyedBin:    samoyedBin,
		DirewolfBin:   direwolfBin,
		WorkDir:       wd,
		Verbose:       *verbose,
		Logger:        logger,
		RecordDir:     *recordDir,
		RecordOnStart: *recordDir != "",
		RTPriority:    *rtPriority,
	})
	if err != nil {
		logger.Error("start router", "err", err)
		os.Exit(1)
	}

	if len(compositePorts) > 0 {
		path, err := r.StartCompositeRecording(compositePorts)
		if err != nil {
			logger.Error("start composite recording", "err", err)
			_ = r.Stop()
			os.Exit(1)
		}
		logger.Info("composite recording", "file", path, "channels", *composite)
	}

	// Run until SIGINT/SIGTERM or a child dies (router cancels its own ctx).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		logger.Info("signal received, shutting down")
		cancel()
	}()

	r.Wait()
	if err := r.Stop(); err != nil {
		logger.Error("stop", "err", err)
	}
}

func resolveSamoyed(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if p, err := exec.LookPath("samoyed-direwolf"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		"/opt/samoyed/dist/samoyed-direwolf",
		"/usr/local/bin/samoyed-direwolf",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("samoyed-direwolf not found in $PATH or common locations")
}

func resolveDirewolf(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if p, err := exec.LookPath("direwolf"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/bin/direwolf", "/usr/local/bin/direwolf"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("direwolf not found in $PATH or common locations")
}

// parseCompositePorts turns a comma-separated "a.vhf,b.vhf" list into
// PortRefs. Empty input yields no refs (composite disabled). Order is
// preserved: the first port is channel 0 (left ear in the stereo case).
func parseCompositePorts(s string) ([]config.PortRef, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var refs []config.PortRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dot := strings.IndexByte(part, '.')
		if dot <= 0 || dot >= len(part)-1 {
			return nil, fmt.Errorf("invalid -composite port %q (want <node>.<port>)", part)
		}
		refs = append(refs, config.PortRef{NodeID: part[:dot], PortID: part[dot+1:]})
	}
	return refs, nil
}

func resolveWorkDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, os.MkdirAll(explicit, 0o755)
	}
	wd, err := os.MkdirTemp("", "sim-router-")
	if err != nil {
		return "", err
	}
	return wd, nil
}
