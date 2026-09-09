# beads_watch

Read and write [beads](https://github.com/Dicklesworthstone/beads_rust) from any
machine on the tailnet, without ssh.

`beads_watch` is a pipe to `br`. It runs the real binary in the real repo and
hands back what it said, byte for byte, with its exit code intact.

**It does not know what a bead is.** No projection, no cache, no model of an
issue, no hand-written route per subcommand. That is the entire design:

- `br ready` over HTTP is correct *by construction*, because it is `br ready`.
- Nothing can drift, because there is no second implementation to drift from.
- A `br` upgrade adds features here for free.

A previous attempt reimplemented beads semantics in ~200k lines of Rust. Its
`/ready` returned 172 issues on a repo where `br ready` returned 96; the two
predicates had quietly diverged. This daemon cannot develop that bug.

**Contents**: [Quick example](#quick-example) · [Architecture](#architecture) ·
[API](#api) · [Identity](#identity) · [Security](#security) ·
[Events](#events) · [Install](#install) · [CLI](#cli) ·
[Configuration](#configuration) · [Troubleshooting](#troubleshooting) ·
[Limitations](#limitations) · [FAQ](#faq)

## Quick example

Every command below is a real transcript against a running daemon.

```console
$ curl -s --unix-socket /run/user/1000/beads_watch.sock http://local/v1/health
{ "node": "dev", "ok": true, "repos": 5, "version": "0.1.0" }

# or over the tailnet, through caddy
$ curl -s https://beads-dev.dev.a44.io/v1/health
{ "node": "dev", "ok": true, "repos": 5, "version": "0.1.0" }
```

Run a `br` subcommand in a named repo. The body is `br`'s stdout, unchanged:

```console
$ curl -s https://beads-dev.dev.a44.io/v1/repos/beads_watch/br \
    -H 'Content-Type: application/json' \
    -d '{"args":["ready","--limit","3"]}' | jq -r '.[].title'
Switch tailscale serve from localhost TCP to the unix socket
One bad repo path in config crash-loops the whole daemon
Forwarded identity is trusted from ANY peer
```

Writes work the same way, and the audit trail records who did it:

```console
$ curl -s https://beads-dev.dev.a44.io/v1/repos/beads_watch/br \
    -d '{"args":["create","Fix the thing","-p","2","-t","bug"]}'
```

Try it without installing anything, against a repo on this box:

```console
beads_watch --repo demo=~/dev/beads_watch --listen 127.0.0.1:7717
curl -s 127.0.0.1:7717/v1/repos/demo/br -d '{"args":["stats"]}'
```

### Why use it

| | |
|---|---|
| **No second implementation** | The daemon shells out to `br`. There is no query layer to disagree with the CLI. |
| **Exit codes survive** | `br`'s typed exit code comes back in `X-Br-Exit`, so a failed `br` still reaches the client as `br` output. |
| **Identity from the transport** | `BD_ACTOR` is set from what the proxy observed, not from what the caller claimed. |
| **Free upgrades** | New `br` subcommands are reachable the moment the binary changes, unless the allowlist excludes them. |
| **Works from a phone** | It is HTTPS on the tailnet. Any client that can POST JSON is a beads client. |
| **Events without polling** | Opt in, and every mutation lands on an ntfy topic within a second. |

## Architecture

```
any tailnet machine / phone
   │  https://beads-arch.dev.a44.io
   ▼
caddy on pi          wildcard *.dev.a44.io cert (DNS-01), tailnet-bound
   │  reverse_proxy 100.110.83.42:8438
   ▼
systemd-socket-proxyd   (beads_watch-proxy.socket, tailscale IP only)
   │  →  unix:/run/user/1000/beads_watch.sock
   ▼
beads_watch
   │  exec: br <args...>   (cwd = repo, hostile env scrubbed)
   ▼
.beads/
```

Caddy on pi does the transport: HTTPS with a real wildcard cert, listening
only on the tailnet address. On each serving box a `systemd-socket-proxyd`
bridge accepts TCP on that box's tailscale IP and forwards to the 0600 unix
socket, so the daemon itself still writes zero networking, zero auth, zero
TLS. (`tailscale serve` used to play caddy's role; the caddy names are
stable, cover every box under one cert, and don't need root on the box.)

When repos live on other machines, there is still no hub to build. Each
machine runs this same binary behind its own bridge, and caddy names it
`beads-<node>.dev.a44.io`: `beads-arch`, `beads-dev`, and so on. **The
caddy sites directory is the directory.**

## API

Every response carries `X-Beads-Watch-Version` and `X-Beads-Watch-Node`, so a
client can always tell that *this daemon* answered, and which box it was.

### `GET /v1/health`

Liveness. Runs no `br`, so it stays up even when a repo is broken.

```json
{ "node": "dev", "ok": true, "repos": 5, "version": "0.1.0" }
```

### `GET /v1/repos`

The repos this node serves. Per-repo detail comes from `br where` itself. The
daemon does not know a repo's prefix or database path, so it asks:

```json
{
  "name": "beads_watch",
  "path": "/home/goku/dev/beads_watch",
  "ok": true,
  "where": {
    "path": "/home/goku/dev/beads_watch/.beads",
    "prefix": "bw",
    "database_path": "/home/goku/dev/beads_watch/.beads/beads.db",
    "jsonl_path": "/home/goku/dev/beads_watch/.beads/issues.jsonl"
  }
}
```

Because the answer comes from `br`, a `.beads/redirect` resolves correctly
here and shows up as a `redirected_from` field. A repo whose path is unusable
comes back `"ok": false` with an `error` instead of `where`; the listing does
not fail as a whole.

### `GET /v1/whoami`

Reports exactly which identity headers arrived and what the daemon made of
them. Tagged devices and user-owned nodes behave differently, so this exists
to settle the question by observation instead of assumption:

```json
{
  "identity": { "actor": "dev", "source": "tailscale-whois", "verified": true },
  "tailscale_headers": {},
  "user_header_forwarded": false,
  "peer_addr": "100.70.239.127",
  "remote_addr": "@"
}
```

Reach for it first whenever a write lands under the wrong `created_by`.

### `POST /v1/repos/{repo}/br`

```json
{ "args": ["ready", "--limit", "20"], "actor": "optional-fallback" }
```

The response body is `br`'s stdout, unmodified. Metadata rides in headers:

| Header | Meaning |
|---|---|
| `X-Br-Exit` | `br`'s own typed exit code, passed through |
| `X-Br-Repo` | the repo that ran |
| `X-Br-Duration-Ms` | how long `br` took |
| `X-Br-Actor` | what `BD_ACTOR` was set to, if anything |
| `X-Br-Actor-Source` | `tailscale-user-header`, `tailscale-whois`, `request`, or `none` |
| `X-Br-Actor-Verified` | whether the transport vouched for the actor |
| `X-Br-Stderr` | base64 of `br`'s stderr, when non-empty |
| `X-Br-Stdout-Truncated` | set when output hit the size cap |

### The one rule for clients

**`X-Br-Exit` present means `br` ran, and the body is `br`'s own output.**

HTTP 200 is returned whenever `br` ran at all, including when it failed. `br`
failing is data, not a daemon error. Flattening exit 3 into HTTP 404 would
erase the difference between not-found, validation, and dependency-cycle, so
the code is passed through untranslated:

```console
$ curl -sD- -X POST .../v1/repos/cell/br -d '{"args":["show","nonexistent-999"]}'
HTTP/1.1 200 OK
X-Br-Exit: 3

{ "error": { "code": "ISSUE_NOT_FOUND", "message": "Issue not found: nonexistent-999", … } }
```

A **daemon-level** failure returns 4xx/5xx, carries no `X-Br-Exit`, and is
marked `"source": "beads_watch"` so its origin is never ambiguous:

```json
{ "error": { "code": "COMMAND_NOT_ALLOWED", "message": "…", "source": "beads_watch" } }
```

Two `br` shape gotchas the daemon deliberately does not paper over: `br list`
is an object with rows at `.issues[]` while `br ready`/`br search` are arrays;
and a partial-batch failure writes **two** JSON documents to stdout. Parse with
`jq -s` when that is possible.

### Failure modes

| Condition | What a client sees |
|---|---|
| Daemon down, proxy up | **HTTP 502, empty body**, no `X-Beads-Watch-Version` |
| Proxy not configured | TLS/connection error |
| `br` exceeded the timeout | HTTP 504, `BR_TIMEOUT` |
| Repo not served here | HTTP 404, `REPO_NOT_FOUND` |
| Subcommand off the allowlist | HTTP 403, `COMMAND_NOT_ALLOWED` |
| `--db` or `--actor` in args | HTTP 403, `FLAG_NOT_ALLOWED` |
| Malformed body, empty args | HTTP 400, `BAD_REQUEST` / `EMPTY_ARGS` |

The 502-with-empty-body case is the one to branch on for a fallback to local
`br`. Checking for the `X-Beads-Watch-Version` header distinguishes "this
daemon answered" from "something else did".

## Identity

`BD_ACTOR` is set per request from what the proxy says about the caller, so the
audit trail records who did what from where.

Resolution order:

1. **`Tailscale-User-Login`**: the proxy must *strip* a client-supplied
   copy, so its presence means the proxy itself put it there. `tailscale
   serve` does this natively; caddy does not, so the beads site files carry
   an explicit `header_up -Tailscale-User-Login`. Without it, any tailnet
   caller can forge a verified actor (demonstrated live, then fixed).
2. **WhoIs on `X-Forwarded-For`**: the daemon takes the *last* entry, which
   is the proxy's own view of the peer. Caddy *appends* its observed client
   address, so a caller's forged prefix loses; `tailscale serve` *replaces*
   the header outright. Either way the address that wins is
   transport-observed, then resolved with `tailscale whois`.
3. **`actor` in the request body**: self-asserted, recorded as unverified.
4. **Nothing**: `BD_ACTOR` is left unset and `br` uses its own default.

All three proxy behaviours were verified by experiment (tailscale 1.98.9;
caddy via the live `beads-arch.dev.a44.io` chain), not assumed. A request
from `dev` through caddy resolves to `actor: dev`, `source:
tailscale-whois`, `verified: true`, and one carrying a forged
`Tailscale-User-Login: attacker@evil.com` or `X-Forwarded-For: 9.9.9.9`
still does. A caller that reaches the bridge port directly, skipping caddy,
can still assert either header unchallenged; see [Limitations](#limitations).

**On this tailnet, user identity is not available.** 13 of 15 nodes are
`tagged-devices`, which have no owning user for tailscale to report. So the
actor is the *machine*: a write from `dev` records `created_by: "dev"`. That is
a real, transport-verified fact rather than an invented name. For the same
reason, the daemon reports `"source": "none"` instead of guessing when it
knows nothing.

## Security

The daemon binds a unix socket at mode `0600`. The bridge
(`systemd-socket-proxyd`, same user) connects to it while other local users
are shut out; the bridge's own TCP port binds only the tailscale IP, so the
LAN never sees it.

Two `br` flags are refused anywhere in `args`, because each breaks a promise
the URL makes:

- **`--db`** would read a different workspace than the repo in the path,
  producing a well-formed answer about the wrong repo.
- **`--actor`** would forge the audit identity that the forwarded identity
  exists to establish.

The allowlist is matched against the first token of `args`. Absent by default
and addable only by a deliberate config edit: `upgrade` (replaces the `br`
binary, which is remote code execution), `init`, `delete`, `doctor` (`--repair`
mutates), `config`, `history`, and the `completions`/`help` shell plumbing.

The daemon also scrubs `BEADS_DIR`, `BD_DB`, `BD_DATABASE`, `BD_ACTOR`,
`BR_OUTPUT_FORMAT`, and `TOON_DEFAULT_FORMAT` from `br`'s environment.
`BEADS_DIR` sits *above* cwd-walking in `br`'s resolution order, so a daemon
started from a shell that exports it would serve every repo from one workspace
while still reporting the requested repo's name. Nothing in the response would
signal the mistake. There is a regression test for exactly this.

> `--listen` makes the daemon bind TCP *instead of* the socket. Prefer the
> socket plus the proxyd bridge: TCP is reachable by any local user, and the
> daemon cannot serve both at once, so the pair of units beats the flag.

## Events

With an opt-in `notify` block in the config, the daemon also tails every
served repo's beads mutation log and publishes each typed event (created,
claimed, commented, assigned, closed, …) to an ntfy topic within a second,
tagged by event type, repo, node, and actor. Assignments also land on per-agent
dispatch topics. The daemon replays `br`'s own audit trail through a cursor
rather than inferring anything, so the no-second-implementation rule holds.
The full contract, delivery semantics, and subscriber recipes live in
[EVENTS.md](EVENTS.md).

## Install

Needs Go 1.24+ and `br` on `PATH`. The tailnet path also needs tailscale and a
caddy host. The module is not published for `go install`; build from a clone:

```bash
git clone https://github.com/a44-io/beads_watch.git
cd beads_watch
go build -o ~/.local/bin/beads_watch .
beads_watch --version
```

Write a config at `~/.config/beads_watch/config.json`:

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "repos": [
    { "name": "cell", "path": "/home/goku/cell" },
    { "name": "beads_watch", "path": "/home/goku/dev/beads_watch" }
  ]
}
```

Check it before wiring systemd. `--print-config` applies every default and
validates every repo path without binding anything:

```bash
beads_watch --print-config
```

Run it under systemd. The units in `systemd/` are made to be symlinked:

```bash
ln -s ~/dev/beads_watch/systemd/beads_watch.service \
      ~/dev/beads_watch/systemd/beads_watch-proxy.socket \
      ~/dev/beads_watch/systemd/beads_watch-proxy.service \
      ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now beads_watch beads_watch-proxy.socket
loginctl enable-linger "$USER"   # so it survives logout
```

Edit `ListenStream` in `beads_watch-proxy.socket` to this box's own
`tailscale ip -4` first. Then name it from pi, which takes one line because the
wildcard cert already covers it:

```bash
ssh pi caddy/expose beads-<node> <tailscale-ip>:8438
```

and copy the `header_up -Tailscale-User-Login` /
`header_up -Tailscale-User-Name` strips from an existing `beads-*.caddy`
into the generated site file (see [Identity](#identity); without them the
verified actor is forgeable).

Verify all three hops:

```bash
curl -s --unix-socket /run/user/1000/beads_watch.sock http://local/v1/health
curl -s "$(tailscale ip -4 | head -1):8438/v1/health"
curl -s https://beads-<node>.dev.a44.io/v1/health
```

## CLI

The daemon takes no subcommands. Flags override the config file.

| Flag | Meaning |
|---|---|
| `--config <path>` | Config file. Default `$XDG_CONFIG_HOME/beads_watch/config.json`. |
| `--socket <path>` | Unix socket to bind, overriding the config. |
| `--listen <addr>` | Bind TCP *instead of* the socket, e.g. `127.0.0.1:7717`. |
| `--repo name=path` | Serve a repo ad hoc. Repeatable, and **replaces** the config file entirely. |
| `--timeout <sec>` | Per-request `br` timeout. Default 30. |
| `--print-config` | Print the effective config, with defaults applied, and exit. |
| `--version` | Print the version and exit. |

`--repo` is the quickest way to try the daemon against one repo without
touching `~/.config`; note that it means the config file is not read at all.

## Configuration

Unknown keys are a hard error rather than a warning, so a typo fails startup
instead of silently doing nothing.

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "repos": [
    { "name": "cell", "path": "/home/goku/cell" }
  ],

  "allow": ["ready", "show", "list"],
  "br_path": "/home/goku/.local/bin/br",
  "timeout_seconds": 30,
  "lock_timeout_ms": 5000,
  "max_output_bytes": 8388608,
  "inject_json": true,

  "notify": {
    "url": "https://ntfy.example.ts.net",
    "topic": "beads-events",
    "token_file": "/home/goku/.config/beads_watch/ntfy-token"
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `socket` | `$XDG_RUNTIME_DIR/beads_watch.sock` | Unix socket to bind. |
| `repos[].name` | basename of `path` | URL path segment. An alias, so a repo can be renamed on the wire without moving on disk. |
| `repos[].path` | required | Directory `br` runs in. `br` walks up from here to find `.beads`. |
| `allow` | the read commands plus create/claim/close | **Replaces** the default allowlist; it does not extend it. |
| `br_path` | `br` on `PATH` | The `br` binary to exec. |
| `timeout_seconds` | `30` | Per-request `br` timeout. |
| `lock_timeout_ms` | `5000` | `--lock-timeout` passed to `br` when the caller did not. |
| `max_output_bytes` | `8388608` (8 MiB) | Stdout cap; past it the response sets `X-Br-Stdout-Truncated`. |
| `inject_json` | `true` | Add `--json` when the caller picked no format. |
| `notify` | absent (off) | Event publishing. Keys documented in [EVENTS.md](EVENTS.md). |

## Troubleshooting

### HTTP 502 with an empty body

The bridge is up and the daemon is not. Check it, and note that a crash-looping
unit reports `activating (auto-restart)` rather than `failed`, so it does not
show up in `systemctl --failed`:

```bash
systemctl --user status beads_watch
journalctl --user -u beads_watch -n 50
```

### `stat /path/to/repo: no such file or directory`, over and over

A configured repo path is gone, and startup validation currently treats that as
fatal, so one dead entry takes every healthy repo down with it and the unit
restarts every 2s forever. Remove the entry (or restore the directory) and
restart:

```bash
beads_watch --print-config     # fails on the offending repo, names it
systemctl --user restart beads_watch
```

If the path exists when *you* look but not for the service, the unit's sandbox
is hiding it: `PrivateTmp=true` gives the service its own `/tmp`, and
`ProtectHome`/`ProtectSystem` restrict the rest. The error text is identical in
both cases, which makes this one easy to misread.

### `json: unknown field "..."` at startup

The config rejects unknown keys deliberately. Check the spelling against
[Configuration](#configuration); `--print-config` shows what actually parsed.

### Writes land under the wrong `created_by`, or `"source": "none"`

Ask the daemon what it saw:

```bash
curl -s https://beads-<node>.dev.a44.io/v1/whoami | jq .
```

`source: none` through caddy usually means the site file is missing the
`header_up -Tailscale-User-Login` strip, or `X-Forwarded-For` is not reaching
the daemon. On a tagged device an actor of the *machine* name is correct rather
than a bug; see [Identity](#identity).

### HTTP 403 `COMMAND_NOT_ALLOWED`

The subcommand is off the allowlist. The error's `hint` lists everything that
is allowed. Remember that setting `allow` in the config replaces the default
list rather than adding to it.

### The daemon will not start: "address already in use"

A previous process still holds the socket. `beads_watch` refuses to steal a
socket from a live daemon but clears a stale one, so this means something is
genuinely listening:

```bash
systemctl --user status beads_watch
ss -lx | grep beads_watch
```

### `br` works in a shell but returns the wrong repo's data

Almost always `BEADS_DIR` exported in the environment. The daemon scrubs it,
and the service file unsets it; a manual run from a polluted shell is the case
to check.

## Limitations

- **One bad repo path stops everything.** Startup stats every configured path
  and exits on the first failure, so a deleted repo crash-loops the daemon and
  takes the healthy repos with it. Tracked in beads.
- **Forwarded identity is trusted from any peer.** A caller that reaches the
  bridge port directly, skipping caddy, can set `Tailscale-User-Login` or
  `X-Forwarded-For` and mint a verified actor. `systemd-socket-proxyd` erases
  the TCP peer before the unix socket, so the daemon cannot tell caddy from a
  direct caller. Exposure is tailnet-only; the fix is a trusted-proxy list,
  which needs the daemon to see the real peer. Tracked in beads.
- **`--listen` is exclusive, not additive.** The daemon serves a socket or a
  TCP address, never both, which is why the proxyd bridge exists.
- **No auth, no TLS, no rate limiting of its own.** Every bit of that is the
  transport's job. Exposed outside a tailnet, this daemon is an unauthenticated
  remote shell onto the allowlisted `br` surface.
- **No user identity on tagged devices.** Tailscale reports no owning user for
  them, so the actor is the machine name.
- **Responses are buffered, not streamed,** and capped by `max_output_bytes`.
  A long `br` run returns nothing until it finishes.
- **No `-C` flag in `br` 0.2.22.** Repos are targeted by setting the child's
  working directory, so anything that changes how `br` walks up from cwd
  changes what the daemon serves.
- **Not packaged.** No release binaries, no package manager, no installer;
  clone and `go build`.

## FAQ

### Why not just ssh?

ssh works and needs no daemon, but it needs shell access on every box, a key
per client, and a login shell on a phone. This is one HTTPS endpoint per node
that any JSON client can hit, with the audit actor filled in from the
transport rather than from whoever's key was used.

### Why does a failed `br` return HTTP 200?

Because the request reached `br` and `br` answered. Nothing went wrong at the
transport layer. `br`'s exit codes are typed (not-found, validation,
dependency-cycle), and collapsing them into HTTP status codes would destroy
that distinction. The code comes back in `X-Br-Exit`; see [the one rule for
clients](#the-one-rule-for-clients).

### Does it cache anything?

No. Every request is an exec of the real binary against the real repo. That is
the whole point: a cache is a second implementation, and second
implementations drift.

### What happens when I upgrade `br`?

New subcommands become reachable immediately, subject to the allowlist; changed
output shapes reach clients unmodified. The daemon has no schema of its own to
migrate. `upgrade` itself is off the allowlist, so a client cannot do this
remotely.

### Can two nodes serve the same repo?

Yes, and they will disagree unless the underlying `.beads` is shared or synced,
because each node runs `br` against its own working copy. The daemon has no
notion of a cluster; the caddy sites directory is the only registry.

### Why a unix socket instead of TCP?

Mode `0600` shuts out every other local user on a box running agent swarms.
Localhost TCP does not. The proxyd bridge exists to add tailnet reachability
without giving that up.

### Why is my actor the machine name and not a person?

Tagged devices have no owning user for tailscale to report. The machine name is
a transport-verified fact; a person's name would be a guess. See
[Identity](#identity).

### How do I see what the daemon is actually running with?

`beads_watch --print-config` for the effective config, `GET /v1/repos` for what
each repo resolves to, and `GET /v1/whoami` for what it thinks of the caller.

## Notes

`br` writes serialize on `.beads/.write.lock`. The daemon passes
`--lock-timeout` (default 5000ms) when the caller did not, since agent swarms
write these repos concurrently and waiting beats a client retry loop.

`--json` is injected only when the caller did not choose a format, so
`--format toon` and `--format csv` still work and get an honest `Content-Type`.

`br` 0.2.22 has **no `-C` flag**; the daemon targets a repo by setting the
child process's working directory, which is also what makes `.beads/redirect`
work.
