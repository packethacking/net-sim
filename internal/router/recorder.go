package router

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
)

// recordSession owns one timestamped directory of WAV files — two files
// (.tx.wav, .rx.wav) per port — and is the tap point for the router's
// audio path. The router holds zero or one of these via an atomic
// pointer, so hot toggle is just "swap the pointer".
//
// Each writer is independent: a write error on one port disables that
// stream but does not affect the others, and never blocks the audio
// path.
type recordSession struct {
	dir    string
	logger *slog.Logger

	mu       sync.Mutex
	writers  map[config.PortRef]*portWriters
	closed   bool
	disabled map[config.PortRef]struct{} // streams that have errored and been dropped
}

type portWriters struct {
	tx *audio.WAVWriter
	rx *audio.WAVWriter
}

// newRecordSession creates a fresh subdirectory under base named with the
// current UTC timestamp, then opens one .tx.wav + .rx.wav per port. The
// final directory path is reported via Dir() and logged for the
// operator.
func newRecordSession(base string, refs []config.PortRef, logger *slog.Logger) (*recordSession, error) {
	if base == "" {
		return nil, fmt.Errorf("recorder: base dir is empty")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: mkdir %q: %w", base, err)
	}
	// Compact RFC3339-ish for filenames (no colons — easier to copy-paste).
	// Millisecond precision so a quick Stop → Start cycle in the UI
	// can't overwrite the previous session.
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	dir := filepath.Join(base, stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: mkdir session dir %q: %w", dir, err)
	}

	s := &recordSession{
		dir:      dir,
		logger:   logger,
		writers:  map[config.PortRef]*portWriters{},
		disabled: map[config.PortRef]struct{}{},
	}

	for _, ref := range refs {
		txPath := filepath.Join(dir, fmt.Sprintf("%s.%s.tx.wav", ref.NodeID, ref.PortID))
		rxPath := filepath.Join(dir, fmt.Sprintf("%s.%s.rx.wav", ref.NodeID, ref.PortID))
		tx, err := audio.NewWAVWriter(txPath)
		if err != nil {
			s.closeAll()
			return nil, fmt.Errorf("recorder: open %s: %w", txPath, err)
		}
		rx, err := audio.NewWAVWriter(rxPath)
		if err != nil {
			_ = tx.Close()
			s.closeAll()
			return nil, fmt.Errorf("recorder: open %s: %w", rxPath, err)
		}
		s.writers[ref] = &portWriters{tx: tx, rx: rx}
	}

	logger.Info("recording started", "dir", dir, "ports", len(refs))
	return s, nil
}

// Dir is the absolute path of the per-session subdirectory. Useful for
// status reporting.
func (s *recordSession) Dir() string { return s.dir }

// WriteTX appends one block to ref's TX wav. Errors disable the stream
// for the rest of the session but never propagate to the audio path.
func (s *recordSession) WriteTX(ref config.PortRef, blk audio.Block) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, dead := s.disabled[ref]; dead {
		return
	}
	pw, ok := s.writers[ref]
	if !ok {
		return
	}
	if _, err := pw.tx.Write(blk); err != nil {
		s.logger.Warn("recorder: tx write failed, disabling stream", "port", ref, "err", err)
		s.disabled[ref] = struct{}{}
	}
}

// WriteRX appends one block to ref's RX wav. Same error semantics as
// WriteTX.
func (s *recordSession) WriteRX(ref config.PortRef, blk audio.Block) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if _, dead := s.disabled[ref]; dead {
		return
	}
	pw, ok := s.writers[ref]
	if !ok {
		return
	}
	if _, err := pw.rx.Write(blk); err != nil {
		s.logger.Warn("recorder: rx write failed, disabling stream", "port", ref, "err", err)
		s.disabled[ref] = struct{}{}
	}
}

// Close flushes header sizes and closes every writer. Idempotent.
func (s *recordSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.closeAll()
	if s.logger != nil {
		s.logger.Info("recording stopped", "dir", s.dir)
	}
}

// closeAll closes every writer. The caller holds s.mu (or the session is
// being torn down before being published, in newRecordSession's failure
// paths).
func (s *recordSession) closeAll() {
	for _, pw := range s.writers {
		if pw.tx != nil {
			_ = pw.tx.Close()
		}
		if pw.rx != nil {
			_ = pw.rx.Close()
		}
	}
}
