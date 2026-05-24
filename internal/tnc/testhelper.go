package tnc

import "io"

// NewTestChild constructs a Child with in-memory I/O for unit tests.
// The caller provides the stdin (RX audio into TNC) and txAudio
// (TX audio out of TNC) ends; the test controls what the "TNC" sees
// and emits without spawning a real process.
func NewTestChild(stdin io.WriteCloser, txAudio io.ReadCloser) *Child {
	return &Child{
		stdin:    stdin,
		txReader: txAudio,
		exitC:    make(chan error, 1),
	}
}
