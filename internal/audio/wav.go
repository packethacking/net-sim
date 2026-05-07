package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WAVWriter streams mono 16-bit-LE PCM at audio.SampleRate to a WAV file.
//
// The header is written up-front with placeholder sizes; on Close the two
// size fields (RIFF chunk size and data sub-chunk size) are patched with
// the real byte counts. If the process is killed without Close running,
// the file is still readable as raw int16 LE PCM but media players will
// not know how long the data section is — most will play the first few
// kilobytes and stop.
type WAVWriter struct {
	f       *os.File
	written uint32 // bytes of PCM written so far (data chunk size)
	closed  bool
}

const wavHeaderBytes = 44

// NewWAVWriter creates path and writes the WAV header. The file is left
// open and ready to receive Block bytes via Write.
func NewWAVWriter(path string) (*WAVWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := writeWAVHeader(f, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write wav header: %w", err)
	}
	return &WAVWriter{f: f}, nil
}

// Write appends PCM bytes verbatim. The caller is responsible for handing
// in correctly-formatted little-endian int16 mono samples — Block already
// is.
func (w *WAVWriter) Write(b []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	n, err := w.f.Write(b)
	w.written += uint32(n)
	return n, err
}

// Close patches the two size fields in the header and closes the file.
// Idempotent: calling twice is a no-op on the second call.
func (w *WAVWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	// Best-effort header patch even if a previous Write errored: a partial
	// file with the correct size in the header is still useful.
	if _, err := w.f.Seek(0, io.SeekStart); err == nil {
		_ = writeWAVHeader(w.f, w.written)
	}
	return w.f.Close()
}

// writeWAVHeader writes the canonical 44-byte RIFF/WAVE/fmt /data header
// for mono 16-bit LE PCM at SampleRate. dataSize is the byte length of
// the data chunk's PCM payload (0 at file creation; patched on Close).
func writeWAVHeader(w io.Writer, dataSize uint32) error {
	const (
		numChannels   = 1
		bitsPerSample = 16
		byteRate      = SampleRate * numChannels * (bitsPerSample / 8)
		blockAlign    = numChannels * (bitsPerSample / 8)
		audioFormat   = 1 // PCM
	)
	hdr := make([]byte, 0, wavHeaderBytes)
	hdr = append(hdr, []byte("RIFF")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 36+dataSize)
	hdr = append(hdr, []byte("WAVE")...)
	hdr = append(hdr, []byte("fmt ")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 16) // fmt chunk size
	hdr = binary.LittleEndian.AppendUint16(hdr, audioFormat)
	hdr = binary.LittleEndian.AppendUint16(hdr, numChannels)
	hdr = binary.LittleEndian.AppendUint32(hdr, SampleRate)
	hdr = binary.LittleEndian.AppendUint32(hdr, byteRate)
	hdr = binary.LittleEndian.AppendUint16(hdr, blockAlign)
	hdr = binary.LittleEndian.AppendUint16(hdr, bitsPerSample)
	hdr = append(hdr, []byte("data")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, dataSize)
	_, err := w.Write(hdr)
	return err
}
