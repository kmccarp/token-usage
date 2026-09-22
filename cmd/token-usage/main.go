// Command token-usage indexes Claude Code and Codex transcripts and serves a
// dashboard of token usage against each provider's rate-limit windows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kmccarp/token-usage/internal/limits"
	limclaude "github.com/kmccarp/token-usage/internal/limits/claude"
	limcodex "github.com/kmccarp/token-usage/internal/limits/codex"
	"github.com/kmccarp/token-usage/internal/model"
	"github.com/kmccarp/token-usage/internal/server"
	"github.com/kmccarp/token-usage/internal/source"
	"github.com/kmccarp/token-usage/internal/source/claudecode"
	"github.com/kmccarp/token-usage/internal/source/codex"
	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

var version = "dev"

// config is the optional JSON file at $XDG_CONFIG_HOME/token-usage/config.json.
type config struct {
	WorkspaceRules []workspace.Rule `json:"workspace_rules"`
}

func main() {
	home, _ := os.UserHomeDir()
	var (
		listen      = flag.String("listen", "tailscale,localhost", "comma-separated addresses to listen on; 'tailscale' resolves to this node's Tailscale IPv4, 'localhost' to 127.0.0.1, or give host:port / :port")
		port        = flag.Int("port", 8787, "port for the named listeners")
		dataDir     = flag.String("data", filepath.Join(home, ".local", "share", "token-usage"), "directory for the index database")
		claudeDir   = flag.String("claude-dir", filepath.Join(home, ".claude"), "Claude Code home")
		codexDir    = flag.String("codex-dir", filepath.Join(home, ".codex"), "Codex home")
		configPath  = flag.String("config", filepath.Join(home, ".config", "token-usage", "config.json"), "optional config file")
		scanEvery   = flag.Duration("scan-interval", 15*time.Second, "how often to look for new transcript data")
		limitsEvery = flag.Duration("limits-interval", 2*time.Minute, "how often to fetch Claude usage limits")
		reindex     = flag.Bool("reindex", false, "drop the index and rebuild it from the transcripts")
		once        = flag.Bool("once", false, "scan once, print stats, and exit (no server)")
		noLimits    = flag.Bool("no-limits", false, "do not call the Claude usage endpoint")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	cfg := loadConfig(*configPath)
	ws, err := workspace.New(cfg.WorkspaceRules)
	if err != nil {
		log.Fatalf("workspace rules: %v", err)
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	st, err := store.Open(filepath.Join(*dataDir, "index.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if *reindex {
		log.Printf("reindex: clearing index")
		if err := st.Reset(); err != nil {
			log.Fatalf("reset: %v", err)
		}
	}

	sources := []source.Source{
		&claudecode.Source{Root: filepath.Join(*claudeDir, "projects")},
		&codex.Source{Root: filepath.Join(*codexDir, "sessions")},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &server.Server{Store: st, Version: version}
	for _, s := range sources {
		srv.Sources = append(srv.Sources, server.SourceInfo{Name: s.Name(), Label: s.Label()})
	}

	var scanMu sync.Mutex
	scanAll := func(ctx context.Context) {
		if !scanMu.TryLock() {
			return
		}
		defer scanMu.Unlock()
		srv.SetScanning(true)
		defer srv.SetScanning(false)
		for _, s := range sources {
			res, err := s.Scan(ctx, st, ws)
			if err != nil {
				log.Printf("scan %s: %v", s.Name(), err)
			}
			for _, e := range res.Errors {
				log.Printf("scan %s: %s", s.Name(), e)
			}
			if res.FilesChanged > 0 || err != nil {
				log.Printf("scan %s: %d files seen, %d changed, %d lines, %d events in %s", s.Name(), res.FilesSeen, res.FilesChanged, res.Lines, res.Events, res.Duration.Round(time.Millisecond))
			}
			srv.RecordScan(s.Name(), res)
		}
	}
	srv.Scan = scanAll

	log.Printf("initial scan starting")
	scanAll(ctx)
	if *once {
		stats, err := st.Stats(ctx)
		if err != nil {
			log.Fatal(err)
		}
		b, _ := json.MarshalIndent(stats, "", "  ")
		fmt.Println(string(b))
		return
	}

	var providers []limits.Provider
	if !*noLimits {
		providers = append(providers, &limclaude.Provider{ClaudeDir: *claudeDir, Every: *limitsEvery})
	}
	providers = append(providers, &limcodex.Provider{Store: st})
	poller := limits.NewPoller(providers, func(l model.Limits) {
		if l.Source == "claude" && l.Raw != nil {
			if raw, ok := l.Raw.(json.RawMessage); ok {
				_ = st.SaveLimitSnapshot("claude", l.FetchedAt.UnixMilli(), string(raw))
			}
		}
	})
	srv.Poller = poller
	go poller.Run(ctx)

	go func() {
		t := time.NewTicker(*scanEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				scanAll(ctx)
			}
		}
	}()

	handler := srv.Handler()
	addrs := resolveListen(*listen, *port)
	if len(addrs) == 0 {
		log.Fatalf("no listen addresses resolved from %q", *listen)
	}
	var servers []*http.Server
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("listen %s: %v", addr, err)
			continue
		}
		hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, hs)
		log.Printf("listening on http://%s", ln.Addr())
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("serve %s: %v", addr, err)
			}
		}()
	}
	if len(servers) == 0 {
		log.Fatal("no listeners could be opened")
	}
	<-ctx.Done()
	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(shutCtx)
	}
}

func loadConfig(path string) config {
	var cfg config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Printf("config %s: %v (ignored)", path, err)
	}
	return cfg
}

// resolveListen expands the -listen spec into concrete host:port addresses.
func resolveListen(spec string, port int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "localhost":
			add(fmt.Sprintf("127.0.0.1:%d", port))
		case part == "all":
			add(fmt.Sprintf(":%d", port))
		case part == "tailscale":
			ip := tailscaleIP()
			if ip == "" {
				log.Printf("tailscale IP not found; skipping tailscale listener (use -listen all to bind every interface)")
				continue
			}
			add(fmt.Sprintf("%s:%d", ip, port))
		case strings.Contains(part, ":"):
			add(part)
		default:
			add(fmt.Sprintf("%s:%d", part, port))
		}
	}
	return out
}

func tailscaleIP() string {
	candidates := []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/usr/local/bin/tailscale"}
	for _, c := range candidates {
		bin := c
		if !strings.Contains(c, "/") {
			p, err := exec.LookPath(c)
			if err != nil {
				continue
			}
			bin = p
		}
		out, err := exec.Command(bin, "ip", "-4").Output()
		if err != nil {
			continue
		}
		// the CLI sometimes prints advice instead of an address; only accept a real IPv4
		for _, f := range strings.Fields(string(out)) {
			if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	// fall back to any 100.64.0.0/10 address on an interface
	ifaces, _ := net.InterfaceAddrs()
	for _, a := range ifaces {
		if ipn, ok := a.(*net.IPNet); ok {
			ip4 := ipn.IP.To4()
			if ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] < 128 {
				return ip4.String()
			}
		}
	}
	return ""
}
