package devpod

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kmccarp/token-usage/internal/store"
)

const bump = 30 * time.Minute

func TestDecideLeavesAnIdleDevpodToStop(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Minute
	w := Workspace{Running: true, BuildID: "b1", Deadline: t0.Add(5 * time.Minute)}
	st := &pollState{}
	read := func(now time.Time) bool {
		if decide(now, w, st, interval, bump) {
			st.lastRead = now
			return true
		}
		return false
	}

	if !read(t0) {
		t.Fatal("the first sight of a build is read")
	}
	w.Deadline = t0.Add(bump + time.Minute) // our own read bumped it
	if read(t0.Add(5 * time.Minute)) {
		t.Fatal("read inside the interval")
	}
	if !read(t0.Add(10 * time.Minute)) {
		t.Fatal("the read after activity stopped is skipped")
	}
	w.Deadline = t0.Add(10*time.Minute + bump + time.Minute)
	for m := 20; m < 40; m++ {
		if read(t0.Add(time.Duration(m) * time.Minute)) {
			t.Fatalf("read at +%dm keeps an idle devpod awake", m)
		}
	}
}

func TestDecideKeepsReadingWhileSomethingElseKeepsItAwake(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	st := &pollState{build: "b1", lastRead: t0, final: true}
	w := Workspace{Running: true, BuildID: "b1", Deadline: t0.Add(10*time.Minute + bump)} // keep-alive at +10m
	if !decide(t0.Add(11*time.Minute), w, st, 10*time.Minute, bump) {
		t.Fatal("a devpod extended by something else is not read")
	}
	if st.final {
		t.Fatal("activity should re-arm the final read")
	}
}

func TestDecideSkips(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if decide(t0, Workspace{BuildID: "b1"}, &pollState{}, time.Minute, bump) {
		t.Fatal("read a stopped devpod")
	}
	st := &pollState{build: "b1", lastRead: t0.Add(-time.Hour)}
	if decide(t0, Workspace{Running: true, BuildID: "b1", Deadline: t0.Add(time.Hour)}, st, time.Minute, 0) {
		t.Fatal("read again with the bump unknown")
	}
	if !decide(t0, Workspace{Running: true, BuildID: "b2"}, st, time.Minute, 0) {
		t.Fatal("a new build should be read once even with the bump unknown")
	}
}

const claudeLine = `{"type":"assistant","sessionId":"s","cwd":"/home/coder/work/x","timestamp":"2026-11-02T12:00:00.000Z","message":{"id":"%s","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":40}}}` + "\n"

const sessionID = "b64f7976-7bcd-4eed-a88f-43a558a5fe6e"

// frames builds what the fetch script prints.
func frames(t *testing.T, parts ...[3]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, p := range parts {
		fmt.Fprintf(gz, "FRAME %s %d %s 1793625456.25 0\n%s\n%s", p[1], len(p[2]), p[0], "/home/coder/.claude/projects/-w/"+sessionID+".jsonl", p[2])
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeClient struct {
	workspaces []Workspace
	bump       time.Duration
	run        func(script string) ([]byte, error)
	scripts    []string
}

func (f *fakeClient) List(context.Context) ([]Workspace, error) { return f.workspaces, nil }
func (f *fakeClient) ActivityBump(context.Context, string) (time.Duration, error) {
	return f.bump, nil
}
func (f *fakeClient) Run(_ context.Context, _, script string, read func(io.Reader) error) error {
	f.scripts = append(f.scripts, script)
	out, err := f.run(script)
	if err != nil {
		return err
	}
	return read(bytes.NewReader(out))
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestReadIsIncrementalAndNamesTheDevpod(t *testing.T) {
	st := openStore(t)
	one := fmt.Sprintf(claudeLine, "m1")
	two := fmt.Sprintf(claudeLine, "m2")
	path := "/home/coder/.claude/projects/-w/" + sessionID + ".jsonl"
	listing := fmt.Sprintf("/home/coder\n%d\t1793625456.25\t%s\n5\t1.0\t/home/coder/.claude/projects/-w/x/tool-results/skip.jsonl\n", len(one), path)
	body := frames(t, [3]string{fmt.Sprint(len(one)), "0", one})
	fc := &fakeClient{bump: bump, workspaces: []Workspace{{ID: "ws1", Name: "pod", TemplateID: "t", BuildID: "b1", Running: true}}}
	fc.run = func(script string) ([]byte, error) {
		if strings.Contains(script, "find \"$HOME/.claude") {
			return []byte(listing), nil
		}
		return body, nil
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := &Sync{Store: st, Client: fc, TempDir: t.TempDir(), Now: func() time.Time { return now }}
	ctx := context.Background()
	s.Pass(ctx)
	if len(fc.scripts) != 2 {
		t.Fatalf("want a list and a fetch, got %d scripts", len(fc.scripts))
	}
	if strings.Contains(fc.scripts[1], "skip.jsonl") {
		t.Fatal("fetched a file that is not a transcript")
	}
	stats, err := st.GroupBy(ctx, "workspace", store.Filter{Source: "claude"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Key != "devpod:pod" || stats[0].Totals.Total != 100 {
		t.Fatalf("groups = %+v", stats)
	}

	// Nothing changed and the machine went on: no fetch.
	fc.scripts = nil
	now = now.Add(11 * time.Minute)
	fc.workspaces[0].Deadline = now.Add(bump * 2)
	s.Pass(ctx)
	if len(fc.scripts) != 1 {
		t.Fatalf("unchanged files were fetched: %d scripts", len(fc.scripts))
	}

	// The file grew: only the new bytes are asked for.
	listing = fmt.Sprintf("/home/coder\n%d\t1793625999.5\t%s\n", len(one)+len(two), path)
	body = frames(t, [3]string{fmt.Sprint(len(one) + len(two)), fmt.Sprint(len(one)), two})
	fc.scripts = nil
	now = now.Add(11 * time.Minute)
	fc.workspaces[0].Deadline = now.Add(bump * 2)
	s.Pass(ctx)
	if want := fmt.Sprintf("frame '%s' %d", path, len(one)); len(fc.scripts) != 2 || !strings.Contains(fc.scripts[1], want) {
		t.Fatalf("scripts = %q, want %q", fc.scripts, want)
	}
	stats, _ = st.GroupBy(ctx, "workspace", store.Filter{Source: "claude"}, 0)
	if stats[0].Totals.Total != 200 {
		t.Fatalf("groups = %+v", stats)
	}
}

// TestScriptsOnGNU runs the real scripts in sh; BSD find has no -printf.
func TestScriptsOnGNU(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the scripts use GNU find")
	}
	home := t.TempDir()
	st := openStore(t)
	cp := filepath.Join(home, ".claude/projects/-w/"+sessionID+".jsonl")
	dp := filepath.Join(home, ".codex/sessions/2026/11/02/rollout-2026-11-02T12-00-00-01a0c606-cf74-7032-8370-d74014ae3dd8.jsonl")
	for _, p := range []string{cp, dp} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	codexMeta := `{"timestamp":"2026-11-02T12:00:00.000Z","type":"session_meta","payload":{"id":"01a0c606-cf74-7032-8370-d74014ae3dd8","cwd":"/home/coder/work/y","thread_source":"user"}}` + "\n" +
		`{"timestamp":"2026-11-02T12:00:01.000Z","type":"turn_context","payload":{"cwd":"/home/coder/work/y","model":"gpt-5.5"}}` + "\n"
	codexTokens := `{"timestamp":"2026-11-02T12:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"total_tokens":110}}}}` + "\n"
	write := func(p, s string) {
		t.Helper()
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	write(cp, fmt.Sprintf(claudeLine, "m1")+`{"type":"assistant","partial`)
	write(dp, codexMeta+codexTokens)

	fc := &fakeClient{bump: bump, workspaces: []Workspace{{ID: "ws1", Name: "pod", TemplateID: "t", BuildID: "b1", Running: true}}}
	fc.run = func(script string) ([]byte, error) {
		cmd := exec.Command("sh", "-s")
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = strings.NewReader(script)
		cmd.Stderr = os.Stderr
		return cmd.Output()
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := &Sync{Store: st, Client: fc, TempDir: t.TempDir(), Now: func() time.Time { return now }}
	ctx := context.Background()
	s.Pass(ctx)
	totals := func(source string) int64 {
		t.Helper()
		g, err := st.GroupBy(ctx, "workspace", store.Filter{Source: source}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(g) == 0 {
			return 0
		}
		if g[0].Key != "devpod:pod" {
			t.Fatalf("workspace = %q", g[0].Key)
		}
		return g[0].Totals.Total
	}
	if got := totals("claude"); got != 100 {
		t.Fatalf("claude total = %d", got)
	}
	if got := totals("codex"); got != 110 {
		t.Fatalf("codex total = %d", got)
	}

	// The partial line completes, and the earlier Codex event keeps its key.
	write(cp, `":1}`+"\n"+fmt.Sprintf(claudeLine, "m2"))
	write(dp, codexTokens)
	now = now.Add(11 * time.Minute)
	fc.workspaces[0].Deadline = now.Add(bump * 2)
	s.Pass(ctx)
	if got := totals("claude"); got != 200 {
		t.Fatalf("claude total = %d", got)
	}
	if got := totals("codex"); got != 220 {
		t.Fatalf("codex total = %d", got)
	}
}

func TestParseWorkspaces(t *testing.T) {
	got, err := parseWorkspaces([]byte(`[
	 {"id":"a","name":"one","template_id":"t","latest_build":{"id":"b","status":"running","deadline":"2026-11-02T14:17:51.183303Z"}},
	 {"id":"c","name":"two","template_id":"t","latest_build":{"id":"d","status":"stopped","deadline":"0001-01-01T00:00:00Z"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Running || got[0].Deadline.IsZero() || got[1].Running || !got[1].Deadline.IsZero() {
		t.Fatalf("%+v", got)
	}
}

func TestParseActivityBump(t *testing.T) {
	got, err := parseActivityBump([]byte(`[{"Template":{"id":"t","activity_bump_ms":1800000}}]`), "t")
	if err != nil || got != 30*time.Minute {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := parseActivityBump([]byte(`[]`), "t"); got != 0 {
		t.Fatalf("unknown template = %v", got)
	}
}

func TestParseMTime(t *testing.T) {
	if got := parseMTime("1793625456.2534"); got != 1793625456253 {
		t.Fatal(got)
	}
	if got := parseMTime("1793625456"); got != 1793625456000 {
		t.Fatal(got)
	}
}
