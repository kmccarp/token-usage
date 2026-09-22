// Package workspace maps a working directory to a human-meaningful workspace
// name. The default rules understand cwt worktrees (~/worktrees/<repo>/<ws>)
// and plain checkouts (~/dev/git/<repo>); extra rules can be supplied from
// config as regular expressions with a replacement template.
package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rule maps directories matching Pattern to the workspace named by Name,
// which may reference capture groups ($1, ${name}).
type Rule struct {
	Pattern string `json:"pattern"`
	Name    string `json:"name"`
	re      *regexp.Regexp
}

// Mapper resolves cwd strings to workspace names.
type Mapper struct {
	rules []Rule
}

// New builds a mapper from user rules followed by the built-in defaults.
func New(user []Rule) (*Mapper, error) {
	home, _ := os.UserHomeDir()
	h := regexp.QuoteMeta(home)
	defaults := []Rule{
		{Pattern: `^` + h + `/worktrees/[^/]+/([^/]+)(/|$)`, Name: `$1`},
		{Pattern: `^/private/tmp/claude-[0-9]+/-Users-[^/]+-worktrees-[^/]+-([^/]+?)/[0-9a-f-]{36}(/|$)`, Name: `$1`},
		{Pattern: `^` + h + `/dev/git/([^/]+)(/|$)`, Name: `$1`},
		{Pattern: `^` + h + `/(?:src|code|projects|repos)/([^/]+)(/|$)`, Name: `$1`},
		{Pattern: `^` + h + `/([^/]+)(/|$)`, Name: `~/$1`},
		{Pattern: `^` + h + `$`, Name: `~`},
	}
	all := append(append([]Rule{}, user...), defaults...)
	for i := range all {
		re, err := regexp.Compile(all[i].Pattern)
		if err != nil {
			return nil, err
		}
		all[i].re = re
	}
	return &Mapper{rules: all}, nil
}

// Name returns the workspace for cwd, or a fallback derived from the path.
func (m *Mapper) Name(cwd string) string {
	cwd = filepath.Clean(cwd)
	for _, r := range m.rules {
		if idx := r.re.FindStringSubmatchIndex(cwd); idx != nil {
			var dst []byte
			dst = r.re.ExpandString(dst, r.Name, cwd, idx)
			if s := strings.TrimSpace(string(dst)); s != "" {
				return s
			}
		}
	}
	if cwd == "" || cwd == "." {
		return "(unknown)"
	}
	parts := strings.Split(strings.Trim(cwd, "/"), "/")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return "/" + strings.Join(parts, "/")
}
