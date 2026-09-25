# token-usage

A small server that measures how many tokens your coding agents are using and shows it
against the provider rate-limit windows (the 5-hour and weekly caps). It reads the
transcripts the tools already write to disk, so nothing has to change in how you run them.

Sources today:

| Source      | Reads                                       | Rate limits from                                         |
|-------------|---------------------------------------------|----------------------------------------------------------|
| Claude Code | `~/.claude/projects/**/*.jsonl`             | the OAuth usage endpoint Claude Code uses for `/usage`   |
| Codex       | `~/.codex/sessions/**/rollout-*.jsonl`      | the `rate_limits` block Codex writes into every rollout  |

Sub-agents are included: Claude Code `Agent` launches (one file per agent under the session
directory) and Codex sub-agent threads are attributed to their parent session and can be
drilled into.

Measurement only. It does not recommend anything.

Everything stays on the machine: the index is a SQLite file under `~/.local/share`, the
UI is served by the same binary, and the only outbound call is the Claude usage endpoint
(optional, see [Credentials](#credentials)).

## Requirements

* Go 1.26 or newer to build. No cgo; the SQLite driver is pure Go.
* macOS or Linux. `make install` (launchd) is macOS only; on Linux run the binary under
  systemd or whatever you use, with the same flags.
* Claude Code and/or Codex installed for the current user, so their transcript directories
  exist.

## Install

### 1. Build

```
git clone https://github.com/kmccarp/token-usage.git
cd token-usage
make build
```

That produces `./token-usage`. With Go on your PATH you can skip the clone:

```
go install github.com/kmccarp/token-usage/cmd/token-usage@latest
```

### 2. Try it once

```
./token-usage -listen localhost -port 8787
```

Open http://localhost:8787. The first scan reads every transcript;
after that it only reads what changed, every 15 seconds. Stop it with Ctrl-C. Add
`-no-limits` if you do not want it calling the Claude usage endpoint.

### 3. Run it at login

**macOS (launchd).** `make install` builds the binary, copies it to `~/bin/token-usage`,
writes `~/Library/LaunchAgents/com.kmccarp.token-usage.plist`, and starts it. The agent
starts at login, restarts if it dies, and logs to `~/Library/Logs/token-usage.log`.

```
make install
make uninstall     # stops the agent and removes the plist; binary and index stay
```

`BIN_DIR` and `PORT` override the defaults, for example `PORT=9000 make install`.

To upgrade, pull and run `make install` again. It replaces the binary and restarts the
agent; the index is kept and only new transcript data is read.

**Linux (systemd user unit).** Build, copy the binary somewhere on your PATH, and create
`~/.config/systemd/user/token-usage.service`:

```
[Unit]
Description=token-usage
After=network-online.target

[Service]
ExecStart=%h/bin/token-usage -listen tailscale,localhost -port 8787
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

```
systemctl --user daemon-reload
systemctl --user enable --now token-usage
journalctl --user -u token-usage -f
```

`-listen tailscale` needs the `tailscale` CLI on the service's PATH; if you are not on a
tailnet use `-listen localhost` or `-listen all`.

### 4. Open it from another device

The default `-listen tailscale,localhost` binds the node's Tailscale IPv4 address and
127.0.0.1, so it is reachable from other tailnet devices at `http://<node>:8787` and from
nowhere else. Use `-listen all` to bind every interface. There is no authentication, so do
not expose it beyond a private network.

## Flags

```
-listen           tailscale,localhost | all | host:port | comma-separated (default tailscale,localhost)
-port             8787
-data             ~/.local/share/token-usage   (index.db lives here)
-claude-dir       ~/.claude
-codex-dir        ~/.codex
-config           ~/.config/token-usage/config.json
-scan-interval    15s
-limits-interval  2m      how often to ask Anthropic for the current utilization
-reindex          drop the index and rebuild from the transcripts
-once             scan, print stats, exit
-no-limits        never call the Claude usage endpoint
```

## What the numbers mean

Every row is one API request. `total = input + cache write + cache read + output`, using
the tool's own accounting:

* Claude Code: `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`,
  `output_tokens` from each assistant message. A message written as several transcript
  lines (one per content block) is counted once, by `message.id`.
* Codex: `last_token_usage` from each `token_count` event. Codex reports cached input as
  part of `input_tokens`; it is split out into cache read so the two sources add up the
  same way. `reasoning_output_tokens` is kept as a subset of output.

Limit percentages are the provider's own numbers. The "tokens here" figure under each
window is what this machine sent during that window, so over time you can see how many
local tokens a percent of your allowance costs. It will not match the percentage exactly:
the provider weights models and cache differently, and other devices or products (Claude
chats, Cowork) draw from the same weekly allowance. The Claude card lists the provider's
own product split when it is available.

Workspace names come from the session's working directory. The defaults, in order:

* `~/worktrees/<repo>/<workspace>` → `<workspace>` (git worktree layouts such as cwt)
* `~/dev/git/<repo>`, `~/src/<x>`, `~/code/<x>`, `~/projects/<x>`, `~/repos/<x>` → the repo
* `~/<x>/...` → `~/<x>`
* anything else → the first path components

`~/.config/token-usage/config.json` can add rules, tried before the defaults:

```json
{
  "workspace_rules": [
    { "pattern": "^/Users/alice/clients/([^/]+)/", "name": "client:$1" },
    { "pattern": "^/Users/alice/dev/git/?$", "name": "scheduled-jobs" }
  ]
}
```

Change the rules and run with `-reindex` once; the workspace is stored on each event.

## API

All endpoints take the same filters: `range` (`5h`, `24h`, `today`, `7d`, `30d`, `all`,
or `window:<source>:<key>` to align to a provider window), or explicit `from`/`to` in unix
milliseconds; plus `source`, `workspace`, `session`, `agent`, `model`, `dir`.

| Endpoint            | Returns                                                        |
|---------------------|----------------------------------------------------------------|
| `GET /api/status`   | index stats, sources, last scan                                |
| `GET /api/limits`   | provider windows with utilization, reset time, local totals    |
| `GET /api/summary`  | totals, by source, by model                                    |
| `GET /api/breakdown`| `by=workspace|directory|model|source|session|agent`            |
| `GET /api/timeseries` | `bucket=15m|hour|day`, `by=source|workspace|model`           |
| `GET /api/session`  | `source=&id=`: one session with its agents and models          |
| `POST /api/refresh` | rescan now and refetch limits                                  |

## Adding a source

Implement `source.Source` (see `internal/source/codex` for the smaller example): walk your
tool's files, read new lines from the stored offset, emit `model.Event` rows keyed by
something stable so re-reads are idempotent, and upsert `model.Session` / `model.Agent`
metadata. Register it in `cmd/token-usage/main.go`. If the tool has a rate-limit API or
writes its limit state to disk, add a `limits.Provider` alongside.

## Credentials

The Claude limit fetch reuses Claude Code's own OAuth token, read from the macOS keychain
item `Claude Code-credentials` (or `~/.claude/.credentials.json`, or
`CLAUDE_CODE_OAUTH_TOKEN`). The token is never stored or logged. Pass `-no-limits` to skip
this entirely; local token counts still work.

## What it reads, and what it keeps

* Read: transcript files under `~/.claude/projects` and `~/.codex/sessions`. Only the usage
  counters, timestamps, model, working directory, session titles (the first user message or
  Claude Code's generated title), sub-agent descriptions, and Codex rate-limit blocks are
  kept. Prompt and response text is never stored.
* Written: `~/.local/share/token-usage/index.db` and, for the launchd install,
  `~/Library/Logs/token-usage.log` (request paths and timings only).
* Served: whatever `-listen` binds. The default is the Tailscale address plus loopback.
  There is no authentication; anyone who can reach the port can see session titles and
  directory names, so keep it on a private network.

## License

MIT, see [LICENSE](LICENSE).
