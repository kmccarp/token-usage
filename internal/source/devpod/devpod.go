// Package devpod indexes Claude Code and Codex transcripts from devpods; the README explains why reads are gated.
package devpod

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/kmccarp/token-usage/internal/source"
	"github.com/kmccarp/token-usage/internal/source/claudecode"
	"github.com/kmccarp/token-usage/internal/source/codex"
	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

const (
	// DefaultInterval is well under the 30 minutes a devpod idles before stopping.
	DefaultInterval = 10 * time.Minute

	checkEvery = time.Minute // how often to list devpods, which does not touch them

	// bumpSlack covers the delay before the agent reports our own read as activity.
	bumpSlack = 3 * time.Minute

	readTimeout = 15 * time.Minute
)

// Workspace is one devpod as the Coder server reports it.
type Workspace struct {
	ID         string
	Name       string
	TemplateID string
	BuildID    string
	Running    bool
	Deadline   time.Time // zero when it has no stop time
}

// Client reaches devpods. Only Run connects to the devpod, which counts as activity on it.
type Client interface {
	List(ctx context.Context) ([]Workspace, error)
	// ActivityBump is how far a connection pushes out the stop time, or 0 if unknown.
	ActivityBump(ctx context.Context, templateID string) (time.Duration, error)
	// Run runs the script in sh on the devpod and hands its stdout to read.
	Run(ctx context.Context, name, script string, read func(io.Reader) error) error
}

// Sync reads devpods into the store.
type Sync struct {
	Store    *store.Store
	Client   Client
	Interval time.Duration
	TempDir  string
	OnPass   func(source.ScanResult) // optional, called after a pass that read something

	Now func() time.Time // for tests

	polls   map[string]*pollState
	lastErr string
}

// pollState is what decide remembers about one devpod.
type pollState struct {
	build    string
	lastRead time.Time
	final    bool // the read after activity stopped has happened
}

// decide allows a read only if a stop time past lastRead+bump shows something else keeps the devpod awake, plus one final read.
func decide(now time.Time, w Workspace, st *pollState, interval, bump time.Duration) bool {
	if !w.Running {
		return false
	}
	if st.build != w.BuildID {
		*st = pollState{build: w.BuildID}
		return true
	}
	if now.Sub(st.lastRead) < interval {
		return false
	}
	if bump <= 0 {
		return false // can't tell our own bump from anything else's
	}
	if w.Deadline.IsZero() || w.Deadline.After(st.lastRead.Add(bump+bumpSlack)) {
		st.final = false
		return true
	}
	if !st.final {
		st.final = true
		return true
	}
	return false
}

// Run reads devpods until ctx ends.
func (s *Sync) Run(ctx context.Context) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		s.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass reads every devpod that is due, logging a repeated failure only once.
func (s *Sync) Pass(ctx context.Context) {
	start := s.now()
	var res source.ScanResult
	err := s.pass(ctx, &res)
	res.Duration = s.now().Sub(start)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != s.lastErr && msg != "" {
		log.Printf("devpods: %s", msg)
	}
	s.lastErr = msg
	for _, e := range res.Errors {
		log.Printf("devpods: %s", e)
	}
	if res.FilesChanged > 0 {
		log.Printf("devpods: %d files seen, %d changed, %d lines, %d events in %s", res.FilesSeen, res.FilesChanged, res.Lines, res.Events, res.Duration.Round(time.Millisecond))
	}
	if res.FilesSeen > 0 && s.OnPass != nil {
		s.OnPass(res)
	}
}

func (s *Sync) pass(ctx context.Context, res *source.ScanResult) error {
	list, err := s.Client.List(ctx)
	if err != nil {
		return err
	}
	if s.polls == nil {
		s.polls = map[string]*pollState{}
	}
	bumps := map[string]time.Duration{}
	present := map[string]bool{}
	for _, w := range list {
		present[w.ID] = true
		st := s.polls[w.ID]
		if st == nil {
			st = &pollState{}
			s.polls[w.ID] = st
		}
		bump, known := bumps[w.TemplateID]
		if !known {
			if bump, err = s.Client.ActivityBump(ctx, w.TemplateID); err != nil {
				res.Errors = append(res.Errors, err.Error())
			}
			bumps[w.TemplateID] = bump
		}
		now := s.now()
		if !decide(now, w, st, s.interval(), bump) {
			continue
		}
		st.lastRead = now // a failed read may have connected too
		rctx, cancel := context.WithTimeout(ctx, readTimeout)
		err := s.read(rctx, w, res)
		cancel()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", w.Name, err))
		}
	}
	for id := range s.polls {
		if !present[id] {
			delete(s.polls, id)
		}
	}
	return nil
}

func (s *Sync) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sync) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultInterval
}

// ingester is the part of a tool's source that reads a chunk from elsewhere.
type ingester interface {
	Name() string
	Accepts(p string) bool
	Ingest(st *store.Store, ws *workspace.Mapper, c source.Chunk) (lines, events int, err error)
}

// listScript prints $HOME, then size, mtime and path for every transcript.
const listScript = `printf '%s\n' "$HOME"
find "$HOME/.claude/projects" "$HOME/.codex/sessions" -type f -name '*.jsonl' -printf '%s\t%T@\t%p\n' 2>/dev/null
exit 0
`

// fetchFunc prints a header, the path, then exactly the header's byte count, so a growing file stays framed.
const fetchFunc = `frame() {
  f=$1; off=$2
  meta=$(find "$f" -maxdepth 0 -printf '%s %T@' 2>/dev/null) || return 0
  [ -n "$meta" ] || return 0
  size=${meta%% *}; mtime=${meta#* }
  [ "$size" -ge "$off" ] || off=0
  len=$((size - off))
  lines=$(head -c "$off" "$f" | wc -l)
  printf 'FRAME %s %s %s %s %s\n%s\n' "$off" "$len" "$size" "$mtime" "$lines" "$f"
  tail -c +$((off + 1)) "$f" | head -c "$len"
}
`

func (s *Sync) read(ctx context.Context, w Workspace, res *source.ScanResult) error {
	var home string
	var listed []remoteFile
	err := s.Client.Run(ctx, w.Name, listScript, func(r io.Reader) (err error) {
		home, listed, err = parseList(r)
		return err
	})
	if err != nil {
		return fmt.Errorf("list transcripts: %w", err)
	}
	if home == "" {
		return errors.New("list transcripts: no output")
	}
	claude := &claudecode.Source{Root: home + "/.claude/projects"}
	cdx := &codex.Source{Root: home + "/.codex/sessions"}
	pick := func(p string) ingester {
		switch {
		case strings.HasPrefix(p, claude.Root+"/") && claude.Accepts(p):
			return claude
		case strings.HasPrefix(p, cdx.Root+"/") && cdx.Accepts(p):
			return cdx
		}
		return nil
	}
	states := map[string]map[string]store.FileState{}
	for _, in := range []ingester{claude, cdx} {
		if states[in.Name()], err = s.Store.FileStates(in.Name()); err != nil {
			return err
		}
	}
	var script strings.Builder
	script.WriteString("{\n")
	changed := 0
	for _, f := range listed {
		in := pick(f.path)
		if in == nil {
			continue
		}
		res.FilesSeen++
		prev, seen := states[in.Name()][fileKey(w, f.path)]
		if seen && prev.Size == f.size && prev.MTime == f.mtime {
			continue
		}
		off := prev.Offset
		if f.size < off {
			off = 0
		}
		fmt.Fprintf(&script, "frame %s %d\n", shellQuote(f.path), off)
		changed++
	}
	if changed == 0 {
		return nil
	}
	script.WriteString("} | gzip -1\n")
	ws := workspace.Fixed("devpod:" + w.Name)
	return s.Client.Run(ctx, w.Name, fetchFunc+script.String(), func(r io.Reader) error {
		return s.ingestFrames(r, w, ws, pick, states, res)
	})
}

func fileKey(w Workspace, p string) string { return "devpod://" + w.ID + p }

type remoteFile struct {
	path  string
	size  int64
	mtime int64
}

func parseList(r io.Reader) (string, []remoteFile, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() {
		return "", nil, sc.Err()
	}
	home := strings.TrimRight(sc.Text(), "/")
	var out []remoteFile
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, remoteFile{path: parts[2], size: size, mtime: parseMTime(parts[1])})
	}
	return home, out, sc.Err()
}

// parseMTime turns find's seconds.fraction into unix milliseconds.
func parseMTime(s string) int64 {
	secs, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return 0
	}
	frac = (frac + "000")[:3]
	ms, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return n * 1000
	}
	return n*1000 + ms
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ingestFrames spools each frame to an unlinked temp file so no transaction stays open across a network read.
func (s *Sync) ingestFrames(r io.Reader, w Workspace, ws *workspace.Mapper, pick func(string) ingester, states map[string]map[string]store.FileState, res *source.ScanResult) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read frames: %w", err)
	}
	defer gz.Close()
	br := bufio.NewReaderSize(gz, 1<<20)
	spool, err := os.CreateTemp(s.TempDir, "devpod-*")
	if err != nil {
		return err
	}
	defer spool.Close()
	if err := os.Remove(spool.Name()); err != nil {
		return err
	}
	for {
		header, err := br.ReadString('\n')
		if err == io.EOF && header == "" {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read frame: %w", err)
		}
		c, err := parseHeader(header)
		if err != nil {
			return err
		}
		p, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read frame path: %w", err)
		}
		c.chunk.Path = strings.TrimSuffix(p, "\n")
		if err := spool.Truncate(0); err != nil {
			return err
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(spool, br, c.length); err != nil {
			return fmt.Errorf("read %s: %w", c.chunk.Path, err)
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return err
		}
		in := pick(c.chunk.Path)
		if in == nil {
			continue
		}
		c.chunk.Key = fileKey(w, c.chunk.Path)
		c.chunk.Prev = states[in.Name()][c.chunk.Key]
		c.chunk.Body = io.LimitReader(spool, c.length)
		lines, events, err := in.Ingest(s.Store, ws, c.chunk)
		res.Lines += lines
		res.Events += events
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s:%s: %v", w.Name, path.Base(c.chunk.Path), err))
			continue
		}
		res.FilesChanged++
	}
}

type frameHeader struct {
	chunk  source.Chunk
	length int64
}

func parseHeader(line string) (frameHeader, error) {
	bad := fmt.Errorf("bad frame header %q", strings.TrimSpace(line))
	f := strings.Fields(line)
	if len(f) != 6 || f[0] != "FRAME" {
		return frameHeader{}, bad
	}
	var nums [3]int64
	for i, s := range []string{f[1], f[2], f[3]} {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return frameHeader{}, bad
		}
		nums[i] = n
	}
	lines, err := strconv.Atoi(f[5])
	if err != nil {
		return frameHeader{}, bad
	}
	return frameHeader{
		chunk:  source.Chunk{Offset: nums[0], Size: nums[2], MTime: parseMTime(f[4]), LinesBefore: lines},
		length: nums[1],
	}, nil
}
