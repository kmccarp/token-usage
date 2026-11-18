package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func socketPath(t *testing.T) string {
	t.Helper()
	// A socket path has a short length limit, which t.TempDir can exceed on macOS.
	dir, err := os.MkdirTemp("/tmp", "tu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func getOver(t *testing.T, path string) string {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := c.Get("http://token-usage/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func serve(t *testing.T, ln net.Listener, body string) {
	t.Helper()
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) })}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
}

func TestListenSpecKeepsASocketPath(t *testing.T) {
	got := resolveListen("unix:/tmp/x.sock,localhost", 9000)
	if len(got) != 2 || got[0] != "unix:/tmp/x.sock" || got[1] != "127.0.0.1:9000" {
		t.Fatalf("resolveListen = %v", got)
	}
}

func TestServesOverASocketOnlyTheUserCanOpen(t *testing.T) {
	path := socketPath(t)
	ln, err := listenOn(unixPrefix + path)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, ln, "pong")
	if got := getOver(t, path); got != "pong" {
		t.Fatalf("answer over the socket = %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestASocketLeftByADeadProcessIsReplaced(t *testing.T) {
	path := socketPath(t)
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Closing the file descriptor without unlinking is what a killed process leaves.
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	ln, err := listenOn(unixPrefix + path)
	if err != nil {
		t.Fatalf("a stale socket blocked the listener: %v", err)
	}
	serve(t, ln, "new")
	if got := getOver(t, path); got != "new" {
		t.Fatalf("answer = %q", got)
	}
}

func TestASocketSomeoneIsServingIsLeftAlone(t *testing.T) {
	path := socketPath(t)
	first, err := listenOn(unixPrefix + path)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, first, "first")
	if _, err := listenOn(unixPrefix + path); err == nil {
		t.Fatal("a second listener took a socket that was being served")
	}
	if got := getOver(t, path); got != "first" {
		t.Fatalf("the first server stopped answering: %q", got)
	}
}
