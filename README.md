# beads_watch

Read and write [beads_rust](https://github.com/Dicklesworthstone/beads_rust) from any
machine on the tailnet, without ssh.

`beads_watch` is a pipe to `br`. It runs the real binary in the real repo and
hands back what it said, byte for byte, with its exit code intact.

**It does not know what a bead is.** No projection, no cache, no model of an
issue, no hand-written route per subcommand. That is the entire design:

- `br ready` over HTTP is correct *by construction*, because it is `br ready`.
- Nothing can drift, because there is no second implementation to drift from.
- A `br` upgrade adds features here for free.

<p align="center">
  <a href="#quick-example">Quick example</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="#api">API</a> ·
  <a href="#identity">Identity</a> ·
  <a href="#security">Security</a> ·
  <a href="#events">Events</a> ·
  <a href="#install">Install</a> ·
  <a href="#releases">Releases</a> ·
  <a href="#cli">CLI</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#troubleshooting">Troubleshooting</a> ·
  <a href="#limitations">Limitations</a> ·
  <a href="#faq">FAQ</a>
</p>

## Quick example

Every command below is a real transcript against a running daemon.

```console
$ curl -s --unix-socket /run/user/1000/beads_watch.sock http://local/v1/health
{ "node": "dev", "ok": true, "repos": 5, "version": "1.1.0" }

# or over the tailnet, through the reverse proxy
$ curl -s https://beads-dev.example.com/v1/health
{ "node": "dev", "ok": true, "repos": 5, "version": "1.1.0" }
```

Run a `br` subcommand in a named repo. The body is `br`'s stdout, unchanged:

```console
$ curl -s https://beads-dev.example.com/v1/repos/beads_watch/br \
    -H 'Content-Type: application/json' \
    -d '{"args":["ready","--limit","3"]}' | jq -r '.[].title'
Switch tailscale serve from localhost TCP to the unix socket
One bad repo path in config crash-loops the whole daemon
Forwarded identity is trusted from ANY peer
```

Writes work the same way, and the audit trail records who did it:

```console
$ curl -s https://beads-dev.example.com/v1/repos/beads_watch/br \
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
| **Identity from the transport** | `BD_ACTOR` comes from what the proxy observed about the connection; the caller's own claims are ignored. |
| **Free upgrades** | New `br` subcommands are reachable the moment the binary changes, unless the allowlist excludes them. |
| **Works from a phone** | It is HTTPS on the tailnet. Any client that can POST JSON is a beads client. |
| **Events without polling** | Opt in, and every mutation lands on an ntfy topic within a second. |

## Architecture

```
any tailnet machine / phone
   │  https://beads-<node>.example.com
   ▼
reverse proxy        on the proxy box; caddy in the reference setup, with a
                     wildcard *.example.com cert (DNS-01), tailnet-bound
   │  reverse_proxy 100.64.0.10:8438        (a trusted proxy: its headers count)
   ▼
beads_watch          listen 100.64.0.10:8438 (this box's tailscale IP only)
   │                 + unix:/run/user/1000/beads_watch.sock, 0600, for local callers
   │  exec: br <args...>   (cwd = repo, hostile env scrubbed)
   ▼
.beads/
```

The reverse proxy does the transport: HTTPS with a real wildcard cert,
listening only on the tailnet address. On each serving box the daemon binds
that box's own tailscale IP, so the proxy reaches it directly and the daemon
sees the real peer of every connection, which is what lets it believe the
proxy's forwarded identity and nobody else's. The daemon still writes zero
auth and zero TLS. (`tailscale serve` used to play the proxy's role, and a
`systemd-socket-proxyd` bridge used to sit between the proxy and the socket;
the bridge erased the peer, which is why it is gone.)

When repos live on other machines, there is still no hub to build. Each
machine runs this same binary on its own address, and the proxy names it
`beads-<node>.example.com`: `beads-arch`, `beads-dev`, and so on. **The
proxy's sites directory is the directory.**

## API

Every response carries `X-Beads-Watch-Version` and `X-Beads-Watch-Node`, so a
client can always tell that *this daemon* answered, and which box it was.

### `GET /v1/health`

Liveness. Runs no `br`, so it stays up even when a repo is broken.

```json
{ "node": "dev", "ok": true, "repos": 5, "version": "1.1.0" }
```

`repos` counts every configured repo, including any whose path is not usable
on this box right now. When there are some, a `repos_unavailable` count
appears beside it; when there are none, the field is absent. Its presence
tells you at a glance that some `POST /v1/repos/{repo}/br` calls will answer
503.

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
not fail as a whole. A path that is missing outright is reported from a
`stat`, without running `br` in it, so the error reads `stat …: no such file
or directory` rather than a `chdir` failure out of the exec.

### `GET /v1/whoami`

Reports exactly which identity headers arrived and what the daemon made of
them. Tagged devices and user-owned nodes behave differently, and this
endpoint shows which case a caller landed in:

```json
{
  "identity": { "actor": "dev", "source": "tailscale-whois", "verified": true },
  "transport": { "network": "tcp", "peer": "100.64.0.20", "trusted_proxy": true },
  "tailscale_headers": {},
  "user_header_forwarded": false,
  "peer_addr": "100.64.0.30",
  "remote_addr": "100.64.0.20:41022"
}
```

`transport` is what the listener knew before any header arrived: which
listener the connection came in on, the TCP peer, and whether that peer is
in `trusted_proxies`. Identity is decided from it; see [Identity](#identity).
Check this endpoint first whenever a write lands under the wrong
`created_by`.

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
| `X-Br-Actor-Source` | `tailscale-user-header`, `tailscale-whois`, `tailscale-whois-direct`, `request`, or `none` |
| `X-Br-Actor-Verified` | whether the transport vouched for the actor |
| `X-Br-Stderr` | base64 of `br`'s stderr, when non-empty |
| `X-Br-Stdout-Truncated` | set when output hit the size cap |

### The one rule for clients

**`X-Br-Exit` present means `br` ran, and the body is `br`'s own output.**

HTTP 200 is returned whenever `br` ran at all, including when it failed. A
failed `br` is still an answer from `br`, and the client needs to see it as
one. Flattening exit 3 into HTTP 404 would erase the difference between
not-found, validation, and dependency-cycle, so the code is passed through
untranslated:

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
| Repo configured, path unusable on this node | HTTP 503, `REPO_UNAVAILABLE` |
| Subcommand off the allowlist | HTTP 403, `COMMAND_NOT_ALLOWED` |
| `--db` or `--actor` in args | HTTP 403, `FLAG_NOT_ALLOWED` |
| Malformed body, empty args | HTTP 400, `BAD_REQUEST` / `EMPTY_ARGS` |

The 502-with-empty-body case is the one to branch on for a fallback to local
`br`. Checking for the `X-Beads-Watch-Version` header distinguishes "this
daemon answered" from "something else did".

`REPO_UNAVAILABLE` is the other fallback-worthy case: the daemon is up and
knows the repo by name, but the checkout is not on this box, or the unit's
sandbox cannot see it. The path is probed on every request, so a `git clone`
that lands later starts answering with no restart, and a checkout that
disappears is refused rather than handed to `br` to fail on `chdir`.

### `GET /v1/events`

A doorbell. Server-sent events, one per served repo each time its JSONL
export is rewritten, saying "something changed in `<repo>`, go ask `br`"
and nothing else:

```console
$ curl -sN https://beads-arch.example.com/v1/events
: connected

event: change
data: {"repo":"beads_watch","ts":"2026-09-14T21:43:19.419943554Z"}

: ping
```

`repo` is the name exactly as `/v1/repos` lists it; `ts` is when the daemon
noticed, RFC 3339. `?repo=a,b` restricts the stream to those repos; a name
this node does not serve is a 404 envelope. A `: ping` comment every 30s
keeps idle proxies from dropping the connection, and the handler never runs
`br`, so a stream can stay open indefinitely.

Which file it watches matters. A read-only `br list` opens the database's
WAL and lock files for writing and would ring anything watching `.beads/`,
including the daemon's own `/br` handler serving the very client that is
listening. Only the JSONL export moves when `br` mutated something, so the
bell is a fingerprint (size and mtime) of that one file, stat'd once a
second and debounced so a multi-file export rings once. A `br list`, through
the daemon or in a shell, rings nothing.

There is no cursor and no replay: a client that reconnects re-asks `br`
once and is current. For typed per-bead events with delivery guarantees,
use the ntfy feed in [Events](#events); this is the transport-local signal
for a client that already holds a connection and will ask `br` for the
truth.

## Identity

`BD_ACTOR` is set per request from what the transport says about the caller,
so the audit trail records who did what from where.

Trust is decided per *connection*, before any header is read. The daemon
owns its listeners, so it knows which one a connection arrived on and, on
TCP, the peer's real address; nothing in a request can change either. That
splits callers into three cases:

- **A trusted proxy** (its address is in `trusted_proxies`; the caddy box in
  the reference setup). Its forwarded headers are its own word, and are read
  in order:
  1. **`Tailscale-User-Login`**: the proxy must *strip* a client-supplied
     copy, so its presence means the proxy itself put it there. `tailscale
     serve` does this natively; caddy does not, so the beads site files
     carry an explicit `header_up -Tailscale-User-Login`. Without it, any
     tailnet caller could forge a verified actor through caddy.
  2. **WhoIs on `X-Forwarded-For`**: the daemon takes the *last* entry, which
     is the proxy's own view of the peer. Caddy *appends* its observed client
     address, so a caller's forged prefix loses; `tailscale serve` *replaces*
     the header outright. Either way the address that wins is
     transport-observed, then resolved with `tailscale whois`.
- **Any other TCP peer** (a caller that reaches `:8438` directly). Its
  headers are its own claims and are not read at all. Its *address* is the
  transport's fact, so that is what gets resolved, and it lands in the audit
  trail as its own machine: `source: tailscale-whois-direct`, `verified:
  true`. A forged `Tailscale-User-Login: attacker@evil.com` or a forged
  `X-Forwarded-For` appears nowhere in the identity.
- **The unix socket.** Nothing there can vouch for a header, so forwarded
  headers are ignored and only the request body counts.

Then, for every case:

3. **`actor` in the request body**: self-asserted, recorded as unverified.
   The only signal on the socket, and the fallback when a direct TCP peer
   is not a tailnet node (`127.0.0.1` in a `--listen` trial, say).
4. **Nothing**: `BD_ACTOR` is left unset and `br` uses its own default.

The proxy behaviours above were established by experiment (tailscale 1.98.9;
caddy via a live `beads-arch.example.com` chain), and the direct-peer rule
has unit tests against a fake `tailscale whois`. A request from `dev`
through caddy resolves to `actor: dev`, `source: tailscale-whois`,
`verified: true`, and one carrying forged headers still does. The same
forged headers sent straight to `arch:8438` from `dev` resolve to `dev` as
well, by a different route that `X-Br-Actor-Source` records.

`trusted_proxies` takes IPs or CIDRs. An empty list trusts nobody: traffic
through an unlisted proxy is still served, attributed to the proxy *machine*
(that is the peer the transport sees), which is accurate as far as it goes
and also the sign that the list is missing an entry.

**On a tailnet of tagged devices, user identity is not available.** Tagged
devices have no owning user for tailscale to report (13 of the 15 nodes on
the tailnet this was built for are tagged). So the actor is the *machine*: a
write from `dev` records `created_by: "dev"`, which the transport has
verified. For the same reason, when the daemon knows nothing it reports
`"source": "none"` and does not guess.

## Security

The daemon binds two listeners. The unix socket, mode `0600`, is the local
path: other local users are shut out. The TCP address is this box's
tailscale IP, so the LAN never sees it; it is bound with `IP_FREEBIND` so the
daemon comes up at login even before tailscaled has brought the address up,
instead of failing its start budget waiting for it.

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

> The socket is served whenever `socket` is set, which it is by default
> unless only `listen` is given. So `--repo … --listen 127.0.0.1:7717` is a
> TCP-only trial that will not collide with a running daemon's socket, and
> a config that names both serves both.

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

One line, from the latest [GitHub Release](https://github.com/a44-io/beads_watch/releases):

```bash
curl -fsSL https://github.com/a44-io/beads_watch/releases/latest/download/setup.sh | bash
```

That installs a prebuilt binary matching the release's commit, verified by
sha256 against the release's own `SHA256SUMS` and `manifest.json` and, when
`cosign` is on the box, by the Sigstore signature the release workflow put
beside each asset (see [Releases](#releases)), discovers
the `.beads` workspaces on the box, writes the config with this box's own
tailnet address as `listen` and the proxy box (`--proxy-host <name>`,
resolved with `tailscale ip`, or `--trusted-proxy <ip>` to skip the lookup;
without either, `trusted_proxies` is empty and no forwarded identity is
trusted) as `trusted_proxies`, generates the systemd unit, starts it, and
health-checks all of it. Flags go after `bash -s --`: `--yes` takes every
default for an unattended run; `--version v1.2.0` installs that release
instead of the latest; `--require-signature` refuses to install anything
whose signature it cannot verify; `--dry-run` prints the plan and changes
nothing; `--uninstall` reverses it and keeps every `.beads`. See
[Releases](#releases) for what a release contains and how it is cut.

Re-running it is safe. An existing `config.json` is never rewritten, not by
a plain re-run and not by `--force`, which only means "reinstall the binary
even when the installed commit is current". Two things a re-run does do to
it, each named on the way and each preceded by a timestamped
`config.json.bak.<ts>`: it adds a key the current release needs when the
file lacks it (`listen`, `trusted_proxies`), and, only with
`--rewrite-config`, it refreshes the `repos` list from discovery (or from
`--repo` flags), keeping every other key, keeping the name you gave a repo
that is still there, and naming each entry it drops. `--dry-run` says which
of these a run would do. An upgrade from a version that had the
`systemd-socket-proxyd` bridge retires the pair once the config has a
`listen` address, and keeps it, with a warning, until then.

Needs `br` on `PATH`, plus tailscale for the tailnet address. Prebuilts
exist for linux/amd64 and linux/arm64; anywhere else it clones the repo at
the release's tag and builds, which needs Go 1.24+.

From a clone instead, which downloads nothing:

```bash
git clone https://github.com/a44-io/beads_watch.git && cd beads_watch && ./setup.sh
```

Or by hand, if you would rather wire it up yourself. Either of these leaves
a binary with no commit stamp, which is fine; `--version` just says less:

```bash
go install github.com/a44-io/beads_watch@latest   # or, from a clone:
go build -o ~/.local/bin/beads_watch .
beads_watch --version
```

Write a config at `~/.config/beads_watch/config.json`:

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "listen": "100.64.0.10:8438",
  "trusted_proxies": ["100.64.0.20"],
  "repos": [
    { "name": "cell", "path": "/home/goku/cell" },
    { "name": "beads_watch", "path": "/home/goku/dev/beads_watch" }
  ]
}
```

`listen` is this box's own `tailscale ip -4`; `trusted_proxies` is the box
running the reverse proxy. Check it before wiring systemd. `--print-config`
applies every default and validates every repo path without binding
anything:

```bash
beads_watch --print-config
```

Run it under systemd. The unit in `systemd/` is made to be symlinked; the
address lives in the config, so the unit is the same on every box. If you
wire it up by hand and later run `setup.sh`, it replaces the symlink with a
real file and moves any drop-in aside, keeping a timestamped backup.

```bash
ln -s ~/dev/beads_watch/systemd/beads_watch.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now beads_watch
loginctl enable-linger "$USER"   # so it survives logout
```

Then give it a name on the proxy box. With a wildcard cert already in place
that is one site block per node, and the two `header_up` strips are not
optional (see [Identity](#identity); without them a caller could forge a
verified actor *through* the proxy):

```caddyfile
beads-<node>.example.com {
    reverse_proxy 100.64.0.10:8438 {
        header_up -Tailscale-User-Login
        header_up -Tailscale-User-Name
    }
}
```

Verify all three hops:

```bash
curl -s --unix-socket /run/user/1000/beads_watch.sock http://local/v1/health
curl -s "$(tailscale ip -4 | head -1):8438/v1/health"
curl -s https://beads-<node>.example.com/v1/health
```

The second one is a direct TCP peer, so `/v1/whoami` on it should say
`source: tailscale-whois-direct` and name this box, whatever headers you
send; the third should say `tailscale-whois` and `trusted_proxy: true`.

## Releases

Releases live on [GitHub Releases](https://github.com/a44-io/beads_watch/releases).
Each one is a tag, and carries a flat set of assets:

| Asset | What it is |
|---|---|
| `setup.sh` | the installer, stamped with this release's own download URL, so the copy fetched from `releases/latest` stays pinned to the release it came with |
| `beads_watch-linux-amd64.tar.gz`, `beads_watch-linux-arm64.tar.gz` | prebuilt, `CGO_ENABLED=0`. Static, so it does not carry the publishing box's glibc to a consumer |
| `SHA256SUMS` | `sha256sum` format, covering every other asset |
| `manifest.json` | commit, tag, version, and sha256 for everything above; what `setup.sh` reads |
| `<asset>.sigstore.json` | one Sigstore bundle per asset above: the signature, the certificate that names who signed, and the transparency-log entry, in one file |

There is no git bundle on a release: the repo itself is the source, and
`setup.sh` clones it at the release's tag when it has to build.

Two checks apply, and they answer different questions. `sha256sum` answers
whether the bytes you have are the bytes that were uploaded:

```bash
sha256sum -c SHA256SUMS
```

The signature answers who uploaded them. Every release is built by
[`release.yml`](.github/workflows/release.yml), and that workflow signs each
asset with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
using its own identity: no long-lived key anywhere, a certificate minted for
the run that names this repository's workflow file at this tag, and a public
transparency-log entry recording the signing. Swapping a tarball on the
release page, even together with its line in `SHA256SUMS`, cannot produce a
bundle that passes this, because nothing outside that workflow run can mint
that certificate:

```bash
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/a44-io/beads_watch/\.github/workflows/release\.yml@refs/tags/v[0-9]' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

`setup.sh` runs both checks. The sha256 pair is always checked, against
`manifest.json` and against `SHA256SUMS`, and a mismatch is exit 3 with
nothing installed. When `cosign` is on the box it then verifies
`manifest.json` and the tarball it is about to install against the identity
above, and a bundle that does not verify is exit 3 as well. Without `cosign`,
or on a release that carries no bundles, it says so and sha256 decides;
`--require-signature` turns both of those into a refusal.

A release is a tag. Push one and the workflow does the rest:

```bash
git tag -a v1.2.0 -m 'beads_watch v1.2.0'
git push origin v1.2.0
```

It cross-compiles for `linux/amd64` and `linux/arm64`, writes the asset set
above, signs it, and opens the release as a **draft**. Nothing is public
until someone reads the draft on GitHub and presses Publish. The workflow is
`scripts/publish-dist.sh --tag v1.2.0 --release --sign` with the right
identity; the same script run by hand produces an unsigned draft, which the
release notes and `setup.sh` both call out, and it refuses a dirty tree,
because what it builds is `HEAD` and uncommitted changes would be left out
of what gets served with nothing to flag it.

A published binary reports the commit it was built from, which is what makes
the release traceable:

```console
$ beads_watch --version
beads_watch 1.1.0
commit: 0123456789abcdef0123456789abcdef01234567
built:  2026-09-09T13:17:48Z
```

`commit` is the sha the binary was built from (illustrative above); it is
also the `commit` in the release's `manifest.json`. `GET /v1/health` grows a
`commit` field on a published binary too. A source build reports neither,
since unstamped code cannot say which commit it is.

## CLI

The daemon takes no subcommands. Flags override the config file.

| Flag | Meaning |
|---|---|
| `--config <path>` | Config file. Default `$XDG_CONFIG_HOME/beads_watch/config.json`. |
| `--socket <path>` | Unix socket to bind, overriding the config. |
| `--listen <addr>` | TCP address to serve as well, e.g. `127.0.0.1:7717`; overrides `listen`. With no socket configured it is TCP only. |
| `--repo name=path` | Serve a repo ad hoc. Repeatable, and **replaces** the config file entirely. |
| `--timeout <sec>` | Per-request `br` timeout. Default 30. |
| `--print-config` | Print the effective config, with defaults applied, and exit. |
| `--version` | Print the version and exit. |

`--repo` is the quickest way to try the daemon against one repo without
touching `~/.config`; note that it means the config file is not read at all.

## Configuration

Unknown keys are a hard error, so a typo fails startup instead of being
ignored.

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "listen": "100.64.0.10:8438",
  "trusted_proxies": ["100.64.0.20"],
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
| `socket` | `$XDG_RUNTIME_DIR/beads_watch.sock`, unless `listen` is set | Unix socket to bind, mode 0600. Set `listen` without `socket` for TCP only. |
| `listen` | absent (none) | TCP address to bind as well: this box's tailscale IP and port, bound with `IP_FREEBIND` so it works before tailscaled brings the address up at login. |
| `trusted_proxies` | `[]` (nobody) | IPs or CIDRs whose forwarded identity headers are believed. Every other TCP peer is identified by its own address. See [Identity](#identity). |
| `repos[].name` | basename of `path` | URL path segment. An alias, so a repo can be renamed on the wire without moving on disk. |
| `repos[].path` | required | Directory `br` runs in. `br` walks up from here to find `.beads`. A path that is missing on this box is served as unavailable (one startup warning, `ok: false` in `/v1/repos`, 503 on `br`), not treated as a config error; only *every* path missing is fatal. |
| `allow` | the read commands plus create/claim/close | **Replaces** the default allowlist; it does not extend it. |
| `br_path` | `br` on `PATH` | The `br` binary to exec. |
| `timeout_seconds` | `30` | Per-request `br` timeout. |
| `lock_timeout_ms` | `5000` | `--lock-timeout` passed to `br` when the caller did not. |
| `max_output_bytes` | `8388608` (8 MiB) | Stdout cap; past it the response sets `X-Br-Stdout-Truncated`. |
| `inject_json` | `true` | Add `--json` when the caller picked no format. |
| `notify` | absent (off) | Event publishing. Keys documented in [EVENTS.md](EVENTS.md). |

Because unknown keys are fatal, a binary older than 1.1.0 refuses a config
that carries `listen` or `trusted_proxies`. `setup.sh` installs the binary
before it adds either key, so an upgrade through it never hits this; a
hand-managed box should upgrade the binary first.

## Troubleshooting

### HTTP 502 with an empty body

Caddy is up and the daemon behind it is not answering on `listen`. Either
the daemon is down, or it is up but serving the socket only; check that the
config has a `listen` address and the journal shows it on the `serving`
line. A daemon that cannot start (a config it refuses) is retried five times
in a minute and then left `failed`, which is the state that shows up in
`--failed`; a unit still inside those retries reports
`activating (auto-restart)` for a few seconds first:

```bash
systemctl --user --failed
systemctl --user status beads_watch
journalctl --user -u beads_watch -n 50
```

After fixing the cause, clear the start-limit counter before starting it
again, or systemd refuses the start until the minute is up:

```bash
systemctl --user reset-failed beads_watch && systemctl --user start beads_watch
```

### `repo path unusable, serving it as unavailable` in the journal

A configured repo path is gone, or is not a directory. The daemon says so once
at startup, keeps serving every other repo, and answers `503 REPO_UNAVAILABLE`
for that one. Fix it anyway, because a repo missing on one box is the kind of
drift nobody notices: either restore the directory (a `git clone` is enough,
no restart needed, since the path is probed per request) or drop the entry
from the config.

```bash
beads_watch --print-config     # warns on stderr, names each unusable repo
curl -s --unix-socket /run/user/1000/beads_watch.sock http://local/v1/repos \
  | jq '.repos[] | select(.ok | not)'
```

If the path exists when *you* look but not for the service, the unit's sandbox
is hiding it: `PrivateTmp=true` gives the service its own `/tmp`, and
`ProtectHome`/`ProtectSystem` restrict the rest. The error text is identical in
both cases, which makes this one easy to misread; the 503's hint says as much.

### `no servable repos: …` and the unit will not start

Every configured path failed. Unlike one missing repo, this is a config
error, and the daemon refuses to come up and serve nothing. The message lists
each repo with its own reason. Also expect this when the unit's sandbox hides
*all* of them, for instance every repo under a directory the service cannot
see. Fix the config, then `systemctl --user reset-failed beads_watch &&
systemctl --user start beads_watch`.

### `json: unknown field "..."` at startup

The config rejects unknown keys deliberately. Check the spelling against
[Configuration](#configuration); `--print-config` shows what actually parsed.

### Writes land under the wrong `created_by`, or `"source": "none"`

Ask the daemon what it saw:

```bash
curl -s https://beads-<node>.example.com/v1/whoami | jq .
```

Look at `transport` first. Every write through caddy landing as the *proxy
box* (`actor: <the proxy's node name>`, `source: tailscale-whois-direct`,
`trusted_proxy: false`) means caddy's address is not in `trusted_proxies`,
so the daemon is identifying caddy itself instead of reading its headers.
`source: none` with
`trusted_proxy: true` means the headers are not arriving: the site file is
missing the `header_up -Tailscale-User-Login` strip, or `X-Forwarded-For` is
not reaching the daemon. On a tagged device, an actor equal to the *machine*
name is the expected result; see [Identity](#identity).

### HTTP 403 `COMMAND_NOT_ALLOWED`

The subcommand is off the allowlist. The error's `hint` lists everything that
is allowed. Remember that setting `allow` in the config replaces the default
list rather than adding to it.

### The daemon will not start: "address already in use"

A previous process still holds the socket. `beads_watch` refuses to steal a
socket from a live daemon but clears a stale one, so this means something is
really listening:

```bash
systemctl --user status beads_watch
ss -lx | grep beads_watch
```

### `br` works in a shell but returns the wrong repo's data

Almost always `BEADS_DIR` exported in the environment. The daemon scrubs it,
and the service file unsets it; a manual run from a polluted shell is the case
to check.

## Limitations

- **Identity is only as good as `trusted_proxies`.** A proxy left off the
  list is served but identified as itself, so every write through it lands
  as the proxy machine. Loud, but wrong until the list is fixed.
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
- **Linux prebuilts only.** Releases carry linux/amd64 and linux/arm64
  tarballs; anywhere else `setup.sh` builds from source, which needs Go
  1.24+. There is no package manager.

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

### Why a unix socket *and* TCP?

The socket, mode `0600`, is for local callers: it shuts out every other
local user on a box running agent swarms, which localhost TCP would not. The
TCP listener is for the tailnet, and it is the daemon's own rather than a
bridge's because only the process that accepts the connection knows who the
peer is, and that is what tells caddy apart from anyone else.

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
`--format toon` and `--format csv` still work and get a matching `Content-Type`.

`br` 0.2.22 has **no `-C` flag**; the daemon targets a repo by setting the
child process's working directory, which is also what makes `.beads/redirect`
work.

## License

[MIT](LICENSE).
