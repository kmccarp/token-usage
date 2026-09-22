// Package claude fetches Claude subscription rate limits from the OAuth usage
// endpoint Claude Code itself uses for /usage. It reuses Claude Code's stored
// OAuth token, so it needs no extra login: macOS keychain item
// "Claude Code-credentials", ~/.claude/.credentials.json, or the
// CLAUDE_CODE_OAUTH_TOKEN environment variable, in that order.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kmccarp/token-usage/internal/model"
)

const (
	usageURL   = "https://api.anthropic.com/api/oauth/usage"
	betaHeader = "oauth-2025-04-20"
)

// Provider polls the usage endpoint.
type Provider struct {
	ClaudeDir string        // ~/.claude
	Every     time.Duration // poll interval
	Client    *http.Client
	UserAgent string

	mu           sync.Mutex
	blockedUntil time.Time
}

// Source implements limits.Provider.
func (p *Provider) Source() string { return "claude" }

// Interval implements limits.Provider.
func (p *Provider) Interval() time.Duration {
	if p.Every == 0 {
		return 2 * time.Minute
	}
	return p.Every
}

type credentials struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// Token finds the OAuth access token Claude Code is currently using.
func (p *Provider) Token() (token, plan string, err error) {
	if t := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); t != "" {
		return t, "", nil
	}
	var raw []byte
	if runtime.GOOS == "darwin" {
		out, kerr := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if kerr == nil {
			raw = out
		}
	}
	if len(raw) == 0 {
		dir := p.ClaudeDir
		if dir == "" {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, ".claude")
		}
		b, ferr := os.ReadFile(filepath.Join(dir, ".credentials.json"))
		if ferr != nil {
			return "", "", errors.New("no Claude Code credentials found (keychain, ~/.claude/.credentials.json, or CLAUDE_CODE_OAUTH_TOKEN)")
		}
		raw = b
	}
	var c credentials
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &c); err != nil {
		return "", "", fmt.Errorf("parse credentials: %w", err)
	}
	if c.ClaudeAiOauth.AccessToken == "" {
		return "", "", errors.New("credentials have no accessToken")
	}
	if c.ClaudeAiOauth.ExpiresAt > 0 && time.UnixMilli(c.ClaudeAiOauth.ExpiresAt).Before(time.Now()) {
		return c.ClaudeAiOauth.AccessToken, c.ClaudeAiOauth.SubscriptionType, errors.New("Claude Code OAuth token is expired; run any claude command to refresh it")
	}
	return c.ClaudeAiOauth.AccessToken, c.ClaudeAiOauth.SubscriptionType, nil
}

// Fetch implements limits.Provider.
func (p *Provider) Fetch(ctx context.Context) (model.Limits, error) {
	l := model.Limits{Source: p.Source(), FetchedAt: time.Now()}
	p.mu.Lock()
	blocked := p.blockedUntil
	p.mu.Unlock()
	if time.Now().Before(blocked) {
		return l, fmt.Errorf("rate limited by usage endpoint until %s", blocked.Format(time.Kitchen))
	}
	token, plan, err := p.Token()
	if err != nil {
		return l, err
	}
	l.Plan = plan
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return l, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", betaHeader)
	ua := p.UserAgent
	if ua == "" {
		ua = "claude-code/" + claudeVersion()
	}
	req.Header.Set("User-Agent", ua)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return l, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := 5 * time.Minute
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if s, err := strconv.Atoi(ra); err == nil {
				wait = time.Duration(s) * time.Second
			}
		}
		p.mu.Lock()
		p.blockedUntil = time.Now().Add(wait)
		p.mu.Unlock()
		return l, fmt.Errorf("usage endpoint returned 429; backing off %s", wait)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return l, fmt.Errorf("usage endpoint returned %d (token expired or revoked; run any claude command to refresh)", resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return l, fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	parsed, err := parse(body)
	if err != nil {
		return l, err
	}
	l.Windows = parsed.windows
	l.Shares = parsed.shares
	l.Notes = parsed.notes
	l.Raw = json.RawMessage(body)
	return l, nil
}

type window struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type limitEntry struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type usageResponse struct {
	Limits     []limitEntry `json:"limits"`
	ExtraUsage *struct {
		IsEnabled    bool    `json:"is_enabled"`
		MonthlyLimit float64 `json:"monthly_limit"`
		UsedCredits  float64 `json:"used_credits"`
		Currency     string  `json:"currency"`
	} `json:"extra_usage"`
	SevenDayBreakdown *struct {
		WindowStartedAt string `json:"window_started_at"`
		Rows            []struct {
			Key         string  `json:"key"`
			DisplayName string  `json:"display_name"`
			Percent     float64 `json:"percent"`
		} `json:"rows"`
	} `json:"seven_day_breakdown"`
}

type parsed struct {
	windows []model.Window
	shares  []model.Share
	notes   []string
}

// parse turns the usage response into windows. The response has grown over
// time: the newer "limits" array is preferred when present, otherwise the
// flat "five_hour" / "seven_day*" keys are read. Unknown experimental keys are
// ignored.
func parse(body []byte) (parsed, error) {
	var out parsed
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return out, fmt.Errorf("decode usage: %w", err)
	}
	var ur usageResponse
	_ = json.Unmarshal(body, &ur)

	parseTime := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	var weeklyStart time.Time
	if ur.SevenDayBreakdown != nil {
		weeklyStart = parseTime(ur.SevenDayBreakdown.WindowStartedAt)
		for _, r := range ur.SevenDayBreakdown.Rows {
			out.shares = append(out.shares, model.Share{Key: r.Key, Label: r.DisplayName, Percent: r.Percent})
		}
	}

	if len(ur.Limits) > 0 {
		for _, e := range ur.Limits {
			if e.Percent == nil {
				continue
			}
			w := model.Window{Utilization: *e.Percent, ResetsAt: parseTime(e.ResetsAt), Active: e.IsActive}
			switch {
			case e.Kind == "session" || strings.Contains(e.Kind, "five_hour"):
				w.Key, w.Label, w.WindowMinutes = "five_hour", "5-hour", 300
			case e.Group == "weekly" || strings.HasPrefix(e.Kind, "weekly") || strings.HasPrefix(e.Kind, "seven_day"):
				w.Key, w.Label, w.WindowMinutes, w.StartedAt = "seven_day", "Weekly", 7*24*60, weeklyStart
				if e.Scope != nil && e.Scope.Model != nil {
					name := e.Scope.Model.DisplayName
					if name == "" {
						name = e.Scope.Model.ID
					}
					if name != "" {
						w.Scope = name
						w.Key = "seven_day_" + strings.ToLower(strings.ReplaceAll(name, " ", "_"))
					}
				}
				if w.Scope == "" && e.Kind != "weekly_all" && e.Kind != "seven_day" {
					w.Key = e.Kind
					w.Label = "Weekly " + strings.ReplaceAll(strings.TrimPrefix(e.Kind, "weekly_"), "_", " ")
				}
			default:
				w.Key, w.Label = e.Kind, strings.ReplaceAll(e.Kind, "_", " ")
			}
			out.windows = append(out.windows, w)
		}
	} else {
		for key, raw := range top {
			if !strings.HasPrefix(key, "five_hour") && !strings.HasPrefix(key, "seven_day") {
				continue
			}
			var w window
			if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
				continue
			}
			mw := model.Window{Key: key, Label: labelFor(key), Utilization: *w.Utilization, WindowMinutes: minutesFor(key), ResetsAt: parseTime(w.ResetsAt)}
			if strings.HasPrefix(key, "seven_day") {
				mw.StartedAt = weeklyStart
			}
			if key != "seven_day" && strings.HasPrefix(key, "seven_day_") {
				mw.Scope = strings.ReplaceAll(strings.TrimPrefix(key, "seven_day_"), "_", " ")
			}
			out.windows = append(out.windows, mw)
		}
	}
	if ur.ExtraUsage != nil && (ur.ExtraUsage.UsedCredits > 0 || ur.ExtraUsage.IsEnabled) {
		state := "off"
		if ur.ExtraUsage.IsEnabled {
			state = "on"
		}
		out.notes = append(out.notes, fmt.Sprintf("extra usage %s: %.2f of %.2f %s used this month", state, ur.ExtraUsage.UsedCredits, ur.ExtraUsage.MonthlyLimit, ur.ExtraUsage.Currency))
	}
	if len(out.windows) == 0 {
		return out, fmt.Errorf("usage response had no windows: %s", truncate(string(body), 200))
	}
	sortWindows(out.windows)
	return out, nil
}

func labelFor(key string) string {
	switch {
	case strings.Contains(key, "five_hour"):
		return "5-hour"
	case key == "seven_day":
		return "Weekly"
	case strings.HasPrefix(key, "seven_day"):
		return "Weekly"
	}
	return strings.ReplaceAll(key, "_", " ")
}

func minutesFor(key string) int {
	switch {
	case strings.Contains(key, "five_hour"):
		return 300
	case strings.Contains(key, "seven_day"):
		return 7 * 24 * 60
	}
	return 0
}

func sortWindows(ws []model.Window) {
	rank := func(w model.Window) int {
		switch {
		case w.Key == "five_hour":
			return 0
		case w.Key == "seven_day":
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && (rank(ws[j]) < rank(ws[j-1]) || (rank(ws[j]) == rank(ws[j-1]) && ws[j].Key < ws[j-1].Key)); j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

func claudeVersion() string {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return "2.1.0"
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "2.1.0"
	}
	return f[0]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
