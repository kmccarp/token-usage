// Package source defines the scanner interface every tool integration
// implements, plus the incremental line reader they share.
package source

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

// Source scans one tool's on-disk transcripts into the store.
type Source interface {
	// Name is the stable identifier stored on every event, e.g. "claude" or "codex".
	Name() string
	// Label is the display name, e.g. "Claude Code".
	Label() string
	// Scan indexes whatever changed since the last call.
	Scan(ctx context.Context, st *store.Store, ws *workspace.Mapper) (ScanResult, error)
}

// ScanResult reports what one scan pass did.
type ScanResult struct {
	FilesSeen    int           `json:"files_seen"`
	FilesChanged int           `json:"files_changed"`
	Lines        int           `json:"lines"`
	Events       int           `json:"events"`
	Duration     time.Duration `json:"duration"`
	Errors       []string      `json:"errors,omitempty"`
}

// MaxLine bounds a single transcript line. Claude Code lines can contain
// whole files pasted into tool results, so this is generous.
const MaxLine = 256 << 20

// WalkJSONL lists every *.jsonl file under root, following the layout rules
// the caller provides through accept.
func WalkJSONL(root string, accept func(path string) bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, keep going
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if accept == nil || accept(p) {
			out = append(out, p)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

// LineHandler receives one complete line (without the trailing newline).
type LineHandler func(line []byte) error

// ReadNewLines reads complete lines from offset and returns the new offset,
// which is the position just past the last newline consumed. A partial trailing
// line is left for the next pass so writers mid-append are never misparsed.
func ReadNewLines(path string, offset int64, h LineHandler) (int64, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, 0, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset, 0, err
		}
	}
	r := bufio.NewReaderSize(f, 1<<20)
	pos := offset
	n := 0
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			if len(buf) > MaxLine {
				return pos, n, io.ErrShortBuffer
			}
			continue
		}
		if err == io.EOF {
			// partial line, leave for next time
			return pos, n, nil
		}
		if err != nil {
			return pos, n, err
		}
		var line []byte
		if len(buf) > 0 {
			buf = append(buf, chunk...)
			line = buf
		} else {
			line = chunk
		}
		consumed := int64(len(line))
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if err := h(line); err != nil {
				return pos, n, err
			}
		}
		pos += consumed
		n++
		buf = buf[:0]
	}
}

// ParseTS accepts RFC3339 timestamps (with or without fractional seconds) and
// returns unix milliseconds, or 0.
func ParseTS(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
