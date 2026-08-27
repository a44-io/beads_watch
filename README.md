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
`/ready` returned 172 issues on a repo where `br ready` returned 96 — the two
predicates had quietly diverged. This daemon cannot develop that bug.

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
`beads-<node>.dev.a44.io` — `beads-arch`, `beads-dev`, and so on. **The
caddy sites directory is the directory.**

## API

### `GET /v1/health`

Liveness. Runs no `br`, so it stays up even when a repo is broken.

```json
{ "node": "Arch", "ok": true, "repos": 13, "version": "0.1.0" }
```

### `GET /v1/repos`

The repos this node serves. Per-repo detail comes from `br where` itself — the
daemon does not know a repo's prefix or database path, it asks. That is why a
`.beads/redirect` resolves correctly here:

```json
{
  "name": "mc",
  "path": "/home/goku/dev/mc",
  "ok": true,
  "where": {
    "path": "/home/goku/dev/mc/.beads-sync/_beads",
    "redirected_from": "/home/goku/dev/mc/.beads",
    "prefix": "mc"
  }
}
```

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
erase the difference between not-found, validation, and dependency-cycle — so
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

The 502-with-empty-body case is the one to branch on for a fallback to local
`br`. Checking for the `X-Beads-Watch-Version` header distinguishes "this
daemon answered" from "something else did".

## Identity

`BD_ACTOR` is set per request from what the proxy says about the caller, so the
audit trail records who did what from where.

Resolution order:

1. **`Tailscale-User-Login`** — the proxy must *strip* a client-supplied
   copy, so its presence means the proxy itself put it there. `tailscale
   serve` does this natively; caddy does not, so the beads site files carry
   an explicit `header_up -Tailscale-User-Login` — without it any tailnet
   caller can forge a verified actor (demonstrated live, then fixed).
2. **WhoIs on `X-Forwarded-For`** — the daemon takes the *last* entry, which
   is the proxy's own view of the peer. Caddy *appends* its observed client
   address, so a caller's forged prefix loses; `tailscale serve` *replaces*
   the header outright. Either way the address that wins is
   transport-observed, then resolved with `tailscale whois`.
3. **`actor` in the request body** — self-asserted, recorded as unverified.
4. **Nothing** — `BD_ACTOR` is left unset and `br` uses its own default.

All three proxy behaviours were verified by experiment (tailscale 1.98.9;
caddy via the live `beads-arch.dev.a44.io` chain), not assumed. A request
from `dev` through caddy resolves to `actor: dev`, `source:
tailscale-whois`, `verified: true` — and one carrying a forged
`Tailscale-User-Login: attacker@evil.com` or `X-Forwarded-For: 9.9.9.9`
still does. A caller that reaches the bridge port directly, skipping caddy,
can still assert either header unchallenged — that hole and its fix
(trusted-proxy peers, which needs the daemon to see the real TCP peer) are
bead `bw-mub`.

**On this tailnet, user identity is not available.** 13 of 15 nodes are
`tagged-devices`, which have no owning user for tailscale to report. So the
actor is the *machine*: a write from `dev` records `created_by: "dev"`. That is
a real, transport-verified fact rather than an invented name — which is why the
daemon reports `"source": "none"` instead of guessing when it knows nothing.

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
binary — remote code execution), `init`, `delete`, `doctor` (`--repair`
mutates), `config`, `history`.

The daemon also scrubs `BEADS_DIR`, `BD_DB`, `BD_DATABASE`, `BD_ACTOR`,
`BR_OUTPUT_FORMAT`, and `TOON_DEFAULT_FORMAT` from `br`'s environment.
`BEADS_DIR` sits *above* cwd-walking in `br`'s resolution order, so a daemon
started from a shell that exports it would serve every repo from one workspace
while still reporting the requested repo's name — a silent, invisible, totally
wrong answer. There is a regression test for exactly this.

> `--listen` makes the daemon bind TCP *instead of* the socket. Prefer the
> socket plus the proxyd bridge: TCP reachable by any local user, and a
> daemon that cannot serve both at once, are each worse than the pair of
> units. (Making `--listen` additive is part of `bw-mub`.)

## Events

With an opt-in `notify` block in the config, the daemon also tails every
served repo's beads mutation log and publishes each typed event (created,
claimed, commented, assigned, closed, …) to an ntfy topic within a second,
tagged by event type, repo, node, and actor — plus per-agent dispatch topics
for assignments. It replays br's own audit trail through a cursor rather than
inferring anything, so the no-second-implementation rule holds. The full
contract, delivery semantics, and subscriber recipes live in
[EVENTS.md](EVENTS.md).

## Install

```bash
go build -o ~/.local/bin/beads_watch .
```

Config at `~/.config/beads_watch/config.json`:

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "repos": [
    { "name": "cell", "path": "/home/goku/cell" },
    { "name": "mc",   "path": "/home/goku/dev/mc" }
  ]
}
```

Optional keys: `allow` (replaces the default allowlist), `br_path`,
`timeout_seconds` (30), `lock_timeout_ms` (5000), `max_output_bytes`,
`inject_json`.

Run it under systemd — the units in `systemd/` are made to be symlinked:

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
`tailscale ip -4` first. Then name it from pi — one line, wildcard cert
already covers it:

```bash
ssh pi caddy/expose beads-<node> <tailscale-ip>:8438
```

and copy the `header_up -Tailscale-User-Login` /
`header_up -Tailscale-User-Name` strips from an existing `beads-*.caddy`
into the generated site file (see Identity — without them the verified
actor is forgeable).

## Notes

`br` writes serialize on `.beads/.write.lock`. The daemon passes
`--lock-timeout` (default 5000ms) when the caller did not, since agent swarms
write these repos concurrently and waiting beats a client retry loop.

`--json` is injected only when the caller did not choose a format, so
`--format toon` and `--format csv` still work and get an honest `Content-Type`.

`br` 0.2.22 has **no `-C` flag**; the daemon targets a repo by setting the
child process's working directory, which is also what makes `.beads/redirect`
work.
