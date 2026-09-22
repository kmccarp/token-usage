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

## Run

```
make build
./token-usage -listen localhost -port 8787
```

Then open http://localhost:8787. The first scan reads every transcript (a few seconds per GB);
after that it only reads what changed, every 15 seconds.

To run it on the tailnet as a login item (macOS, launchd):

```
make install      # builds, writes ~/Library/LaunchAgents/com.kmccarp.token-usage.plist, starts it
make uninstall
```

The default `-listen tailscale,localhost` binds the node's Tailscale IPv4 address and
127.0.0.1, so it is reachable from other tailnet devices at `http://<node>:8787` and from
nowhere else. Use `-listen all` to bind every interface.

Flags:

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

Workspace names come from the working directory:

* `~/worktrees/<repo>/<workspace>` (cwt) → `<workspace>`
* `~/dev/git/<repo>` → `<repo>`
* anything else → the first path components, or a rule from config

`~/.config/token-usage/config.json` can add rules, tried before the defaults:

```json
{
  "workspace_rules": [
    { "pattern": "^/Users/kevin/clients/([^/]+)/", "name": "client:$1" }
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
