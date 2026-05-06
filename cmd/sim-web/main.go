// sim-web is a tiny integrated web front-end for editing the simulator's
// network topology and controlling a sim-router instance.
//
//	sim-web -addr :8080 -config /opt/sim/network.yaml
//
// Bootstraps with a sensible default two-node network if -config doesn't
// exist yet. The YAML config is the source of truth; the web page just
// edits the file in place and offers Start / Stop / Restart for the
// embedded router.
//
// Stdlib + yaml.v3 only. No frameworks.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/router"
	"gopkg.in/yaml.v3"
)

//go:embed index.html
var assets embed.FS

const defaultConfigYAML = `# Default two-node AFSK1200 network. Edit and click Apply.
mixer_mode: fm_capture
capture_db: 6.0
collision_mode: silence

nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002

links:
  - { from: a.vhf, to: b.vhf, loss_db: 0 }
  - { from: b.vhf, to: a.vhf, loss_db: 0 }
`

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	cfgPath := flag.String("config", "network.yaml", "path to the network YAML")
	samoyed := flag.String("samoyed", "", "samoyed-direwolf binary (default: discover)")
	preload := flag.String("pa-stub", "", "libpa_stub.so for LD_PRELOAD (default: discover)")
	workDir := flag.String("workdir", "", "scratch dir for per-port direwolf.conf (default: temp)")
	autostart := flag.Bool("autostart", false, "start the router immediately on launch")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := bootstrapConfig(*cfgPath); err != nil {
		logger.Error("bootstrap config", "err", err)
		os.Exit(1)
	}

	bin, err := resolveBinary(*samoyed)
	if err != nil {
		logger.Warn("samoyed-direwolf not found at startup; you'll need it before you can Start", "err", err)
	}
	stub, err := resolvePreload(*preload)
	if err != nil {
		logger.Warn("libpa_stub.so not found at startup; you'll need it before you can Start", "err", err)
	}

	app := &app{
		cfgPath:    *cfgPath,
		binaryPath: bin,
		preload:    stub,
		workDir:    *workDir,
		logger:     logger,
	}

	tmpl, err := template.ParseFS(assets, "index.html")
	if err != nil {
		logger.Error("parse template", "err", err)
		os.Exit(1)
	}
	app.tmpl = tmpl

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/config", app.handleConfig)
	mux.HandleFunc("/api/topology", app.handleTopology)
	mux.HandleFunc("/api/start", app.handleStart)
	mux.HandleFunc("/api/stop", app.handleStop)
	mux.HandleFunc("/api/restart", app.handleRestart)
	mux.HandleFunc("/api/status", app.handleStatus)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if *autostart {
		if err := app.start(); err != nil {
			logger.Warn("autostart failed", "err", err)
		}
	}

	go func() {
		logger.Info("sim-web listening", "addr", *addr, "config", *cfgPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logger.Info("signal received, shutting down")
	_ = app.stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

type app struct {
	cfgPath    string
	binaryPath string
	preload    string
	workDir    string
	logger     *slog.Logger
	tmpl       *template.Template

	mu       sync.Mutex
	router   *router.Router
	cancel   context.CancelFunc
	lastErr  string
	lastTime time.Time
}

type pageData struct {
	ConfigPath string
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.Execute(w, pageData{ConfigPath: a.cfgPath}); err != nil {
		a.logger.Error("render", "err", err)
	}
}

// handleConfig: GET returns current YAML (text/plain), PUT writes it after
// validation. We round-trip through config.Parse so a syntactically-bad
// or semantically-invalid file is rejected before it's saved.
func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(a.cfgPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := config.ParseBytes(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Atomic-write so a failed save doesn't leave a half-written file.
		tmp := a.cfgPath + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmp, a.cfgPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// handleTopology is the same content as handleConfig but expressed as
// JSON, for the form-based editor. GET parses the YAML on disk into a
// generic map so the client doesn't need to know our struct layout. PUT
// re-marshals to YAML, runs the strict validator, and saves.
//
// The two endpoints share a file (a.cfgPath) — Raw YAML edits and Form
// edits both end up in the same place. Comments are preserved when
// editing via /api/config (text in, text out) but lost when editing
// via /api/topology (round-trips through map → YAML).
func (a *app) handleTopology(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(a.cfgPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var raw any
		if err := yaml.Unmarshal(b, &raw); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(raw)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		yamlBytes, err := yaml.Marshal(raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := config.ParseBytes(yamlBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tmp := a.cfgPath + ".tmp"
		if err := os.WriteFile(tmp, yamlBytes, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmp, a.cfgPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (a *app) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if err := a.start(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if err := a.stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = a.stop()
	if err := a.start(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeStatus(w)
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.writeStatus(w)
}

type statusResponse struct {
	Running   bool             `json:"running"`
	Ports     []portStatusItem `json:"ports"`
	LastError string           `json:"last_error,omitempty"`
	LastEvent string           `json:"last_event,omitempty"`
}

type portStatusItem struct {
	Node     string `json:"node"`
	Port     string `json:"port"`
	Mode     string `json:"mode"`
	KissPort int    `json:"kiss_port"`
}

func (a *app) writeStatus(w http.ResponseWriter) {
	a.mu.Lock()
	defer a.mu.Unlock()

	resp := statusResponse{
		Running:   a.router != nil,
		LastError: a.lastErr,
	}
	if !a.lastTime.IsZero() {
		resp.LastEvent = a.lastTime.Format(time.RFC3339)
	}

	if a.router != nil {
		// Re-read config so the port list reflects what's currently running.
		if cfg, err := config.Load(a.cfgPath); err == nil {
			for _, n := range cfg.Nodes {
				for _, p := range n.Ports {
					resp.Ports = append(resp.Ports, portStatusItem{
						Node:     n.ID,
						Port:     p.ID,
						Mode:     string(p.Modem.Mode),
						KissPort: p.KissPort,
					})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *app) start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.router != nil {
		return errors.New("already running")
	}
	if a.binaryPath == "" {
		bin, err := resolveBinary("")
		if err != nil {
			a.lastErr = err.Error()
			a.lastTime = time.Now()
			return err
		}
		a.binaryPath = bin
	}
	if a.preload == "" {
		stub, err := resolvePreload("")
		if err != nil {
			a.lastErr = err.Error()
			a.lastTime = time.Now()
			return err
		}
		a.preload = stub
	}
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.lastErr = err.Error()
		a.lastTime = time.Now()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r, err := router.Start(ctx, cfg, router.Options{
		BinaryPath:  a.binaryPath,
		PreloadPath: a.preload,
		WorkDir:     a.workDir,
		Logger:      a.logger,
	})
	if err != nil {
		cancel()
		a.lastErr = err.Error()
		a.lastTime = time.Now()
		return err
	}
	a.router = r
	a.cancel = cancel
	a.lastErr = ""
	a.lastTime = time.Now()
	return nil
}

func (a *app) stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.router == nil {
		return nil
	}
	err := a.router.Stop()
	a.router = nil
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.lastTime = time.Now()
	return err
}

func bootstrapConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(defaultConfigYAML), 0o644)
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

