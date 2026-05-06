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
	"syscall"

	"github.com/M0LTE/net-sim/internal/config"
	"github.com/M0LTE/net-sim/internal/router"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML topology file (required)")
	verbose := flag.Bool("v", false, "verbose / debug logging")
	samoyedPath := flag.String("samoyed", "", "path to samoyed-direwolf (default: search $PATH and common install paths)")
	preloadPath := flag.String("pa-stub", "", "path to libpa_stub.so for LD_PRELOAD (default: search common install paths)")
	workDir := flag.String("workdir", "", "scratch dir for per-port direwolf.conf files (default: a unique subdir of $TMPDIR)")
	flag.Parse()

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "sim-router: -config is required")
		flag.Usage()
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

	bin, err := resolveBinary(*samoyedPath)
	if err != nil {
		logger.Error("locate samoyed-direwolf", "err", err)
		os.Exit(1)
	}

	stub, err := resolvePreload(*preloadPath)
	if err != nil {
		logger.Error("locate libpa_stub.so", "err", err)
		logger.Error("if missing, build with: gcc -shared -fPIC -o /usr/local/lib/libpa_stub.so preload/pa_stub.c")
		os.Exit(1)
	}

	wd, err := resolveWorkDir(*workDir)
	if err != nil {
		logger.Error("workdir", "err", err)
		os.Exit(1)
	}

	logger.Info("starting", "config", *cfgPath, "samoyed", bin, "preload", stub, "workdir", wd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := router.Start(ctx, cfg, router.Options{
		BinaryPath:  bin,
		PreloadPath: stub,
		WorkDir:     wd,
		Verbose:     *verbose,
		Logger:      logger,
	})
	if err != nil {
		logger.Error("start router", "err", err)
		os.Exit(1)
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

func resolveBinary(explicit string) (string, error) {
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

func resolvePreload(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	for _, p := range []string{
		"/usr/local/lib/libpa_stub.so",
		"/opt/sim/preload/libpa_stub.so",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("libpa_stub.so not found")
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
