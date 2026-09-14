# The beads event feed

Every mutation in every watched repo — created, claimed, commented, labeled,
assigned, closed, reopened — published to ntfy within about a second, tagged
by what happened and where, with the full audit row as a JSON body. Anything
that can subscribe to an ntfy topic can now react to beads: a phone, a shell
hook, a quickshell widget, or a Claude session waiting for work.

The feed keeps this daemon's one design rule. **It never decides what
happened.** br records every mutation as a typed row in the workspace's
`events` table (the same rows `br audit log <id>` prints); the daemon replays
that log through a monotonic id cursor. There is no diffing, no inference, no
second implementation of beads semantics to drift. A close initiated locally,
over this daemon's HTTP surface, or by any other tool on the box lands in the
same log and therefore in the same feed.

## Not the doorbell

The daemon also serves `GET /v1/events`, a server-sent-event stream that
rings once per repo each time its JSONL export is rewritten (README, API).
The two are for different clients and should not be confused:

| | ntfy feed (this document) | `GET /v1/events` doorbell |
|---|---|---|
| Says | what happened: type, bead, actor, old and new value | that *something* changed in `<repo>` |
| Transport | an ntfy broker, a topic, a token | the connection the client already holds to the daemon |
| Delivery | at-least-once, cursor-driven, survives restarts | at-most-once, no cursor, no replay |
| Meant for | agents, hooks, phones — anything that reacts to a bead | a client that will re-ask `br` for the truth, like omabeads' notifier |

Both keep the no-second-implementation rule: neither decides what a change
means. The feed replays br's own audit rows; the doorbell only says a file
moved.

## The contract

One firehose topic for the whole tailnet (default `beads-events`). Every node
publishes into it; subscribers slice it server-side with `?tags=` filters.

Tags on every message, each value prefixed with its dimension so filters can
never collide across dimensions:

| Tag | Meaning | Example |
|---|---|---|
| `ev-<type>` | what happened | `ev-closed` |
| `repo-<name>` | which repo (the name this daemon serves it as) | `repo-nt` |
| `node-<node>` | which machine | `node-arch` |
| `actor-<actor>` | who did it (BD_ACTOR / forwarded identity) | `actor-JollyEmber` |
| `agent-<name>` | Tier-1 agent attribution, when br recorded it | `agent-CrimsonFox` |
| `from-<status>` / `to-<status>` | status_changed only | `from-open`, `to-in_progress` |

ntfy tag filtering is AND: `?tags=ev-closed,repo-nt` means "closes in nt".
Values are sanitized to `[A-Za-z0-9_-]` (everything else becomes `_`) and
matching is exact and case-sensitive.

Event types, verified against br 0.2.22 by driving a bead through its whole
lifecycle:

```
created            priority_changed     status_changed (old→new)
assignee_changed   commented            label_added
dependency_added   dependency_removed   closed    reopened
```

A close emits both `status_changed` (`to-closed`) and a semantic `closed`;
hooks should key on the semantic one. Claim = `status_changed` to
`in_progress` plus `assignee_changed`. A type this table does not list still
flows — it just gets the generic title.

The message body is JSON, version 1, additive changes only:

```json
{
  "v": 1,
  "node": "arch",
  "repo": "nt",
  "event_id": 20,
  "issue_id": "nt-xsl",
  "issue_title": "Notify pipeline live test",
  "event_type": "assignee_changed",
  "actor": "JollyEmber",
  "old_value": "",
  "new_value": "TestAgent",
  "comment": "",
  "created_at": "2026-08-27T06:18:28.460271237+00:00",
  "agent_name": "", "harness": "", "model": ""
}
```

`event_id` is the repo's audit-log rowid: unique per repo, strictly
increasing, and the dedup key. `issue_title` and `issue_assignee` are the
issue's *current* state at publish time, joined in for convenience; everything
else is the audit row verbatim. Long fields are cut to fit ntfy's message cap
and flagged `"truncated": true` — the full row is always in the repo via
`br audit log <issue_id>`.

The title is a one-line human summary (`nt-xsl open→in_progress by goku`).
Machines should ignore it and read the body.

## Agent dispatch topics

An assignment additionally publishes to `agent-<assignee>` (prefix
configurable). Give an agent a name and it has a work queue:

```bash
br update nt-875 --assignee GC-TestFox     # → topic agent-GC-TestFox
br create "Fix the flaky test" --assignee GC-TestFox    # also routes
```

br emits no `assignee_changed` for `create --assignee`, so the daemon routes
`created` events by the issue's current assignee — both paths verified live.
The dispatch message carries the same JSON body; its title reads
`assigned nt-875 in nt@arch: <issue title>`.

## Delivery semantics

- **At-least-once, in order per repo.** The cursor only advances past an
  event when its publish (and its dispatch publish, if any) succeeded, so an
  ntfy outage delays the feed instead of dropping events. Retries can
  duplicate; dedup on `(node, repo, event_id)`.
- **First sight of a repo starts at the tip.** History is not replayed at a
  subscriber; it stays queryable in the repo itself.
- **A rebuilt database resets the cursor** (detected by MAX(id) falling below
  it) with a warning, rather than going silent forever.
- **A broken repo sits out and retries every minute.** It never takes the
  daemon or the other repos down.
- Latency is the poll interval (default 1s) — file mtimes are stat'd, and the
  log is only queried when something moved, plus a once-a-minute safety poll.
- Deletes are invisible: br's schema cascades an issue's audit rows away with
  the issue. Renaming a repo in the config starts a fresh cursor at the tip.

## Subscribing

Server-side filtering is on the HTTP API. The `ntfy` CLI subscribes to whole
topics only, so filter client-side there:

```bash
# Stream closes in nt, server-side filtered (auth as in ~/.config/ntfy/client.yml)
curl -sN -H "Authorization: Bearer $TOKEN" \
  "https://ntfy.example.ts.net/beads-events/json?tags=ev-closed,repo-nt"

# Same with the ntfy CLI, filtered client-side
ntfy sub beads-events | jq -c 'select(.tags | index("ev-closed") and index("repo-nt"))'

# One agent's dispatch queue (bare topic — no filter needed)
ntfy sub "agent-$GC_AGENT"
```

Declarative hooks belong in `~/.config/ntfy/client.yml` — `subscribe:` blocks
run a `command:` per message, with `if:` filters on tags — run by the ntfy
client service. A Claude session watches the same stream with the `/ntfy`
skill: point it at `beads-events` (or an `agent-<name>` topic) with an
instruction, and the skill's Monitor executes it when a matching message
arrives.

## Configuration

An opt-in `notify` block in the daemon config. Absent block, unchanged
daemon:

```json
{
  "socket": "/run/user/1000/beads_watch.sock",
  "repos": [ { "name": "nt", "path": "/home/goku/dev/nt" } ],
  "notify": {
    "url": "https://ntfy.example.ts.net",
    "topic": "beads-events",
    "token_file": "/home/user/.config/beads_watch/ntfy-token",
    "repos": ["nt"]
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `url` | — (required) | ntfy server |
| `topic` | — (required) | firehose topic |
| `token` / `token_file` | none | bearer auth; the file (0600) wins |
| `node` | lowercased short hostname | the `node-` tag value |
| `poll_ms` | 1000 (min 200) | file-change poll interval |
| `state_file` | `$XDG_STATE_HOME/beads_watch/events-cursor.json` | cursor persistence |
| `route_assignments` | true | publish to agent dispatch topics |
| `agent_topic_prefix` | `agent-` | dispatch topic prefix |
| `repos` | all served repos | watch a subset, by served name |
| `sqlite_path` | `sqlite3` on PATH | reads go through the sqlite3 CLI, read-only |

Reads use `sqlite3 -readonly` against the path `br where` reports (so
`.beads/redirect` resolves here exactly as everywhere else), and the schema is
probed at startup — a workspace too old to have the events table is skipped
loudly, never guessed at.

## Rolling out to a node

Order matters: the old binary rejects a config containing `notify` (unknown
fields are errors), so ship the binary before the config.

```bash
go build -o ~/.local/bin/beads_watch .
install -m 600 /dev/stdin ~/.config/beads_watch/ntfy-token <<< "tk_..."
# add the notify block to ~/.config/beads_watch/config.json
systemctl --user restart beads_watch
journalctl --user -u beads_watch -n 5   # expect "notifying" + one "events: tailing" per repo
```

Every node publishes to the same firehose with its own `node-` tag; nothing
else coordinates. Subscribers that care about one machine filter on
`node-<name>`, and the rest see the tailnet as a single stream of beads
events — which is the point.
