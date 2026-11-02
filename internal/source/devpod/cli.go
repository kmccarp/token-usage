package devpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CLI is a Client over the coder binary and login the devpod CLI keeps.
type CLI struct {
	ConfigDir string // ~/.config/devpod/coder, with its session and url files
	CacheDir  string // ~/.cache/devpod, with the downloaded coder-<version>
}

// LoggedIn reports whether the devpod CLI has signed in on this machine.
func LoggedIn(configDir string) bool {
	for _, name := range []string{"session", "url"} {
		if fi, err := os.Stat(filepath.Join(configDir, name)); err != nil || fi.Size() == 0 {
			return false
		}
	}
	return true
}

// binary is the newest coder the devpod CLI has downloaded.
func (c *CLI) binary() (string, error) {
	matches, err := filepath.Glob(filepath.Join(c.CacheDir, "coder-*"))
	if err != nil {
		return "", err
	}
	best, bestTime := "", time.Time{}
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if fi.ModTime().After(bestTime) {
			best, bestTime = m, fi.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no coder binary in %s; run any devpod command to download it", c.CacheDir)
	}
	return best, nil
}

func (c *CLI) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin, err := c.binary()
	if err != nil {
		return nil, err
	}
	url, err := os.ReadFile(filepath.Join(c.ConfigDir, "url"))
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CODER_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "CODER_URL="+strings.TrimSpace(string(url)), "CODER_CONFIG_DIR="+c.ConfigDir)
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

func (c *CLI) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := c.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("coder %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// List implements Client.
func (c *CLI) List(ctx context.Context) ([]Workspace, error) {
	out, err := c.output(ctx, "list", "--search", "owner:me template:devpod", "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseWorkspaces(out)
}

func parseWorkspaces(b []byte) ([]Workspace, error) {
	var raw []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		TemplateID  string `json:"template_id"`
		LatestBuild struct {
			ID       string    `json:"id"`
			Status   string    `json:"status"`
			Deadline time.Time `json:"deadline"`
		} `json:"latest_build"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse coder list: %w", err)
	}
	out := make([]Workspace, 0, len(raw))
	for _, r := range raw {
		w := Workspace{ID: r.ID, Name: r.Name, TemplateID: r.TemplateID, BuildID: r.LatestBuild.ID, Running: r.LatestBuild.Status == "running"}
		if d := r.LatestBuild.Deadline; d.Year() > 1 {
			w.Deadline = d
		}
		out = append(out, w)
	}
	return out, nil
}

// ActivityBump implements Client.
func (c *CLI) ActivityBump(ctx context.Context, templateID string) (time.Duration, error) {
	out, err := c.output(ctx, "templates", "list", "-o", "json")
	if err != nil {
		return 0, err
	}
	return parseActivityBump(out, templateID)
}

func parseActivityBump(b []byte, templateID string) (time.Duration, error) {
	var raw []struct {
		Template struct {
			ID             string `json:"id"`
			ActivityBumpMS int64  `json:"activity_bump_ms"`
		} `json:"Template"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return 0, fmt.Errorf("parse coder templates: %w", err)
	}
	for _, r := range raw {
		if r.Template.ID == templateID {
			return time.Duration(r.Template.ActivityBumpMS) * time.Millisecond, nil
		}
	}
	return 0, nil
}

// Run implements Client and never starts a stopped devpod.
func (c *CLI) Run(ctx context.Context, name, script string, read func(io.Reader) error) error {
	cmd, err := c.command(ctx, "ssh", "--disable-autostart", "--usage-app=disable", "--wait=no", name, "--", "sh", "-s")
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	readErr := read(stdout)
	if readErr != nil {
		_ = cmd.Process.Kill()
	} else {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) {
		return fmt.Errorf("coder ssh: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}
