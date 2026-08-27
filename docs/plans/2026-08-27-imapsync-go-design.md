# imapsync-go — Design

**Date:** 2026-08-27
**Status:** Accepted (design), not yet implemented

## 1. Purpose

A Go implementation of the ideas behind Perl `imapsync`: one-way migration and
repeated synchronisation of IMAP accounts. The goal that justifies a rewrite is
**concurrency**. IMAP is strictly request/response per connection, so throughput
comes from running many connections and slicing work across them — folder-parallel
*and* intra-folder parallel, so that a single 200k-message INBOX still saturates
the available connections.

### Non-goals

- Bidirectional sync. The engine is shaped so it could be added later, but v1 is
  source → destination only.
- Byte-for-byte reproduction of imapsync's stdout. Scrapers of the old
  `++++ Statistics ++++` block are not supported.
- Full parity with all ~150 imapsync options in v1.

## 2. Decisions

| Decision | Choice |
|---|---|
| Compatibility | New ergonomic CLI + config file; separate `compat` subcommand translating imapsync flags |
| Option coverage (v1) | Core + power: hosts/auth/TLS, folder selection & mapping, dry-run, size/age filters, `delete2`, subscribe, cache, regex transforms, `--search`, header selection, automap, service presets |
| Primary targets | **iCloud** and **mox**. Dovecot, Gmail, Microsoft 365, Cyrus deferred but explicitly on the roadmap |
| Parallelism | Staged pipeline, two connection pools, AIMD adaptive sizing with manual override |
| Large messages | Hybrid: RAM under a threshold, temp-file spool above it, global in-flight byte budget |
| Buffering | Accepted (no direct source→destination piping) |
| Identity | Four-tier ladder, UID map primary |
| Message stamping | Only when the message lacks a usable `Message-ID` |
| Post-append verify | Available behind `--verify`, off by default |
| State store | SQLite (`modernc.org/sqlite`, pure Go, no cgo) |
| CONDSTORE fast path | Yes, v1 |
| Sync direction | One-way, engine shaped for later bidirectional |
| Folder mapping | `SPECIAL-USE` first, then `NAMESPACE`/delimiter translation, then user rules |
| Destructive-op guard | Yes, with `--force` override |
| CLI framework | Cobra + hand-written imapsync flag translator |
| Config file | Yes, multi-pair, credentials referenced not inlined |
| Output | `log/slog` (text on TTY, JSON when piped) + compact live progress |

## 3. Library verification

Checked against `github.com/emersion/go-imap/v2@v2.0.0-beta.8` source, not from
memory:

| Requirement | Status |
|---|---|
| CONDSTORE | Supported — `FetchOptions.ChangedSince`, `FetchOptions.ModSeq`, `SelectData.HighestModSeq` |
| UIDPLUS / APPENDUID | Supported — `AppendData{UID, UIDValidity}`; `COPYUID`; `UID EXPUNGE` |
| SPECIAL-USE, MOVE, ESEARCH, IDLE, SORT, ENABLE, UNSELECT | Supported |
| **QRESYNC** | **Capability constant only.** No `VANISHED` handling anywhere |
| **MULTIAPPEND** | **Capability constant only.** `Append()` is single-message |
| **COMPRESS** (RFC 4978) | **Absent entirely** |
| `imapserver/imapmemserver` | Present — usable as an in-process test backend |

Consequences:

- QRESYNC moves to post-v1 and would require an upstream contribution.
  CONDSTORE alone delivers most of the repeat-sync win.
- `Append(mailbox string, size int64, ...)` requires the literal length **before**
  the body is written. This vindicates spooling: we append our own *measured*
  byte count rather than trusting `RFC822.SIZE`, which some servers misreport.
- go-imap's client is not safe for concurrent commands. Our one-goroutine-per-
  connection pool makes that structurally impossible rather than merely avoided.

The library sits behind a narrow internal interface from day one, so replacing or
forking it stays cheap.

## 4. Architecture

```
discover → plan → [folder diff] → chunk → fetch ⇉ spool ⇉ append → record → reconcile
              ↑                                                        │
              └──────────────── SQLite state ──────────────────────────┘
```

### 4.1 Connection pools

Two independent pools, `srcPool` and `dstPool`, one goroutine per connection,
never shared concurrently.

The pools are **asymmetric**, and this asymmetry is the core of the design:

- **Source connections are folder-bound while leased.** They hold a `SELECT`, so
  a lease is tied to one mailbox for its duration.
- **Destination connections are folder-agnostic.** `APPEND` takes a mailbox
  argument and requires no `SELECT`. One destination pool therefore serves every
  folder simultaneously and stays hot.

Flag updates and expunges on the destination *do* need a `SELECT`, so those run
in a separate reconcile stage that leases destination connections in folder-bound
mode. Keeping that out of the append hot path is deliberate.

### 4.2 Governor (AIMD)

Per-side, per-host adaptive concurrency:

- **Additive increase** on sustained success.
- **Multiplicative decrease** on `BYE`, `NO [OVERQUOTA]`, `[LIMIT]`,
  `Too many simultaneous connections`, and dial/read timeouts.
- Floor 1, ceiling from `--connections` (`auto` by default).

Expected settling points: iCloud 2–5, mox 30+. The governor exists primarily
because iCloud throttles aggressively and unpredictably.

### 4.3 Backpressure

A **byte-budget semaphore**, not a count semaphore. A worker acquires
`min(RFC822.SIZE, cap)` bytes before fetching and releases on append completion.
Messages under ~1 MiB stay in RAM; larger ones spool to `os.CreateTemp`.

With a count semaphore, 500 concurrent 30 MB fetches is 15 GB of RSS. With a byte
budget, 200 tiny messages and 3 large ones cost proportionally to their actual
size.

## 5. Correctness

### 5.1 The hard problem

`APPEND` is not idempotent, and no transaction spans IMAP and SQLite. A crash in
the window between "server accepted the append" and "we committed the row" causes
a naïve restart to re-append — duplicates. This is the most likely cause of the
duplication seen with stock imapsync on iCloud→mox, which needed `--useuid` plus
`--addheader` to work around.

### 5.2 Identity ladder

1. **UID map (authoritative).** `(uidvalidity1, uid1) → (uidvalidity2, uid2)`,
   written transactionally. Valid only while *both* recorded UIDVALIDITYs still
   match. This is `--useuid`, backed by real transactions instead of filenames.
2. **UIDPLUS makes tier 1 exact.** `APPEND` returns
   `[APPENDUID <uidvalidity> <uid>]`, so the destination UID is known
   authoritatively at write time — no post-append `SEARCH` guessing, which is
   exactly where a cache gets poisoned. Falls back to a targeted `SEARCH` when
   UIDPLUS is unavailable.
3. **Header digest — bootstrap only.** Normalised digest over `Message-ID`,
   `Date`, `Subject`, `From`, `To`, `Cc`. Runs on first sync, on a lost or
   invalidated DB, and after a UIDVALIDITY change, in order to **adopt** messages
   already present on the destination. Never consulted when tier 1 has an answer.
   Legitimate duplicates are handled by multiplicity, not collapsed.
4. **Self-stamping.** When a message has no stable `Message-ID`, stamp
   `X-Imapsync-Go-Id: <hash>` on the copy at append time so tier 3 can find it
   later even without the DB. Adding an unsigned header does not invalidate DKIM,
   which signs a named header list. Only applied when needed.

`--verify` adds a post-append re-fetch asserting size/digest before recording
success. Off by default.

### 5.3 Schema

```sql
CREATE TABLE messages (
  pair_id, folder_id,
  src_uidvalidity, src_uid,
  dst_uidvalidity, dst_uid,      -- NULL until confirmed
  ident_hash,                     -- tier-3 digest
  stamp_id,                       -- tier-4 marker, NULL if unused
  state,                          -- planned | in_flight | done | failed
  size, flags, internaldate,
  PRIMARY KEY (pair_id, folder_id, src_uidvalidity, src_uid)
);

CREATE TABLE folders (
  pair_id, name1, name2,
  src_uidvalidity, dst_uidvalidity,
  src_highestmodseq, last_sync
);
```

### 5.4 Write-ahead, not write-behind

The `in_flight` row is written **and committed before** the append is issued.

On startup, every `in_flight` row is a *suspect*: search the destination for its
`stamp_id` or digest, then either adopt the discovered UID or retry the append.
Recovery is therefore a bounded, targeted `SEARCH` — not a full re-diff.

### 5.5 UIDVALIDITY is a fence

Any change on either side invalidates that folder's rows and forces a tier-3
reconciliation pass. Never a silent re-copy.

### 5.6 CONDSTORE fast path

- Stored `HIGHESTMODSEQ` unchanged → the whole folder is skipped in one command.
- Changed → `FETCH 1:* (FLAGS) (CHANGEDSINCE n)` returns only what moved, so flag
  sync costs a delta rather than a full scan.
- CONDSTORE does not report expunges (that is QRESYNC's job), so new-message
  detection still needs a UID-range diff, and a periodic full UID reconcile runs
  via `--reconcile-every` (default: every 7th run).

## 6. Folders

- **`SPECIAL-USE` (RFC 6154) first.** Map by attribute — `\Sent`, `\Trash`,
  `\Drafts`, `\Junk`, `\Archive` — read straight off `LIST`. This is strictly
  more reliable than imapsync's name heuristics and solves iCloud's
  `Sent Messages` / `Deleted Messages` versus mox's `Sent` / `Trash` directly.
- **`NAMESPACE` (RFC 2342) next** for hierarchy delimiter (`/` versus `.`) and
  prefix translation. Translate, never assume.
- **User `--map` rules last**, as an explicit override.

## 7. Safety

Destructive operations (`--delete2`, `--delete1`, expunge) require explicit
opt-in **and** pass a safety valve: a run that would delete more than a threshold
(default 10%) of a folder aborts with a diff summary unless `--force` is given.

## 8. Surface

### Commands

- `sync` — native interface.
- `compat` — imapsync flag translator, handling Getopt::Long semantics
  (`--noX` negation, `--opt=val`, unambiguous abbreviations).
- `probe` — connect, dump capabilities, measure the connection ceiling, print a
  suggested config. Particularly useful against iCloud.
- `status` — read the state DB.
- `verify` — standalone consistency check.

### Config

```yaml
pairs:
  - name: icloud-to-mox
    source:
      url: "imaps://you@imap.mail.me.com:993"
      password: {env: ICLOUD_APP_PW}
    dest:
      url: "imaps://you@mox.example.net:993"
      password: {keychain: mox-imap}
    concurrency: {source: auto, dest: auto, max_inflight: 512MiB}
    folders: {map: special-use, exclude: ["Notes"]}
    delete2: false
```

Credentials are always *referenced* (env var or keychain), never stored inline.

## 9. Testing

1. **Unit** — `imapserver/imapmemserver` in-process. Fast, hermetic.
2. **Integration** — real **mox in Docker**. It is Go, starts in milliseconds,
   and is an actual target.
3. **Fault injection** — a proxy in front of a real server that replays `BYE`,
   mid-literal disconnects, `NO [OVERQUOTA]`, and stalled literals. This is how
   the AIMD governor and `in_flight` recovery are *proven* rather than hoped
   about; a resumability bug that cannot be reproduced on demand will never be
   fixed.

**Core property test:** *sync twice, assert the second run copies zero messages.*
That single invariant is the tombstone for the duplication bug.

## 10. Milestones

- **M0** — skeleton, config, `probe`, capability negotiation.
- **M1** — single-connection correct one-way sync + SQLite state.
- **M2** — pools, staged pipeline, byte-budget spooling.
- **M3** — AIMD governor + fault-injection suite.
- **M4** — CONDSTORE fast path, flag sync, `SPECIAL-USE` mapping.
- **M5** — `compat` shim, `--delete2` + safety valve, progress UI.
- **Post-v1** — QRESYNC (upstream contribution), MULTIAPPEND batching, COMPRESS,
  and broader server support: Dovecot, Gmail, Microsoft 365, Cyrus.

M1 precedes M2 deliberately. Concurrency bugs layered on an unproven diff are
undebuggable.
