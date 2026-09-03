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

## 3. IMAP library selection

### 3.1 Survey

| Library | Stars | State | Verdict |
|---|---|---|---|
| `emersion/go-imap` **v2** | 2350 (repo) | `v2.0.0-beta.8`, actively pushed | **Chosen** |
| `emersion/go-imap` **v1** | same repo, `v1` branch | stable, maintenance only | Rejected: no CONDSTORE |
| `mjl-/mox/imapclient` | n/a | MIT, actively developed | Rejected as a dependency; kept as a reference |
| `BrianLeishman/go-imap` | 109 | active | Rejected: package-level mutable global state |
| `mxk/go-imap` | 213 | no commits since 2020 | Rejected: unmaintained |
| `evmar/go-imap` | 74 | no commits since 2011 | Rejected: unmaintained |

**v1 is eliminated on a single fact.** Its extensions live in twelve separate
repositories — `appendlimit`, `compress`, `enable`, `id`, `idle`, `metadata`,
`move`, `quota`, `sortthread`, `specialuse`, `uidplus`, `unselect` — and
CONDSTORE is not among them. Most were last touched between 2019 and 2021. The
largest performance lever in this design exists only in v2.

**`BrianLeishman/go-imap` is eliminated on our exact axis.** Its `vars.go`
exposes package-level mutable globals (`Verbose`, `RetryCount`, `DialTimeout`,
`CommandTimeout`, `TLSSkipVerify`) plus a package-global `lastResp` holding the
most recent server response. That races under concurrent connections and makes
it impossible to configure the source and destination endpoints differently
within one process. For a tool built on many simultaneous connections, this is
disqualifying regardless of the library's other merits.

**`mox/imapclient` is rejected as a dependency but retained as a reference.** Its
`parse.go` already handles `VANISHED` and `MODSEQ`, quite possibly making it the
only Go client that understands QRESYNC. However it self-describes as
"primarily for testing the mox IMAP4 server", offers no API stability guarantee,
and pulls in mox's `mlog` and `moxio` packages. It is MIT licensed, so it is a
legitimate implementation reference when we write QRESYNC ourselves.

### 3.2 Capability verification

Checked against `github.com/emersion/go-imap/v2@v2.0.0-beta.8` source:

| Requirement | Status |
|---|---|
| CONDSTORE | Supported — `FetchOptions.ChangedSince`, `FetchOptions.ModSeq`, `SelectData.HighestModSeq` |
| UIDPLUS / APPENDUID | Supported — `AppendData{UID, UIDValidity}`; `COPYUID`; `UID EXPUNGE` |
| SPECIAL-USE, MOVE, ESEARCH, IDLE, SORT, ENABLE, UNSELECT | Supported |
| **QRESYNC** | **Capability constant only.** No `VANISHED` handling anywhere |
| **MULTIAPPEND** | **Capability constant only.** `Append()` is single-message |
| **COMPRESS** (RFC 4978) | **Absent entirely** (v1 has `go-imap-compress`; v2 does not) |
| `imapserver/imapmemserver` | Present — usable as an in-process test backend |

### 3.3 Consequences

- **QRESYNC will not arrive upstream on its own.** PR #423, "Support for QRESYNC
  (RFC 7162) only for the client", was opened in 2021 and closed unmerged; the
  tracking issue `imapclient: support QRESYNC` remains open. Post-v1 QRESYNC
  means we implement it, not that we wait for it.
- `Append(mailbox string, size int64, ...)` requires the literal length **before**
  the body is written. This vindicates spooling: we append our own *measured*
  byte count rather than trusting `RFC822.SIZE`, which some servers misreport.
- go-imap's client is not safe for concurrent commands. Our one-goroutine-per-
  connection pool makes that structurally impossible rather than merely avoided.

### 3.4 Residual risk

v2 has shipped eight beta releases with no stable tag, so API churn is a live
risk. The `internal/imapx` façade is therefore load-bearing rather than
ceremonial: everything the rest of the codebase knows about IMAP passes through
it, so forking, extending or replacing the library stays a contained change.

### 3.5 Capabilities are claims, not guarantees

Two rules, both learned the hard way against iCloud during M0:

**Never send syntax the server did not advertise.** iCloud does not offer
LIST-EXTENDED, and answers `BAD Parse Error` to any `LIST` carrying return
options — including `RETURN (SUBSCRIBED)`, which the library will happily emit
for a non-nil options value. The failure is total: no folder list, no sync. Each
extension's *syntax* must be gated on that extension's own capability, not on a
related one. SPECIAL-USE does not imply LIST-EXTENDED; nor does LIST-STATUS.

**Degrade rather than abort when a claim proves false.** A server that
advertises an extension and then rejects it must not end the run. Where a
cheaper path exists only as an optimisation, falling back to the plain form
costs round trips and nothing else, so `imapx` retries once on a `BAD`/`NO` and
logs a warning. This applies only to optimisations: a rejection on a
correctness-critical command must still fail loudly.

Corollary for the whole project: `--trace` exists because inferring which
command a server disliked from a wrapped error is guesswork. It redacts
credentials so it can be used against real accounts and pasted into bug reports.

### 3.6 Establishment must be bounded by the caller

go-imap runs its own read loop and resets any deadline set on the socket, so
`SetDeadline` cannot bound a login. Worse, `Login().Wait()` ignores
`context.Context` entirely. A server that accepts a connection and then goes
quiet therefore hangs the caller indefinitely — unacceptable for a tool whose
whole premise is hundreds of connections to a throttling server.

`imapx.Dial` races the whole post-connect handshake against the context and
closes the socket on expiry. Any future blocking call added to the façade needs
the same treatment.

Commands are a weaker case. go-imap's command API predates `context` and offers
no way to abandon a command in flight, so `imapx`'s operations check `ctx.Err()`
on entry and honour cancellation *between* commands only. That is enough to stop
a cancelled run from issuing the next few hundred thousand commands, and the
alternative — closing the connection to interrupt a command — is exactly what
§3.7 reserves for genuine desynchronisation.

### 3.7 A short APPEND literal is unrecoverable

APPEND declares its literal's length before sending the bytes, and IMAP has no
way to retract that number. If fewer bytes follow than were promised, the server
goes on reading message data expecting the remainder, no tagged response is ever
sent, and a client that waits for one blocks until the process dies. Measured:
reverting the guard makes the test hang for its full timeout rather than fail.

This makes the "spool with a *measured* byte count" rule from §4 load-bearing for
connection survival, not merely for correctness: `RFC822.SIZE` is a claim servers
get wrong, and trusting it would hang the run. `imapx.Append` verifies the copied
length against the declared one and, on any mismatch, destroys the connection and
returns `ErrConnectionBroken` rather than waiting. A connection is cheap; a hung
sync of 776,747 messages is not.

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

One qualification found in M2: a server that answers `APPEND` without
`APPENDUID` forces a `SEARCH` to find where the message landed, and `SEARCH`
answers only for the selected mailbox. That fallback therefore takes a *second*,
folder-bound lease after releasing the append lease. Holding both at once would
be a destination connection waiting on the destination pool, which deadlocks at
small pool sizes.

### 4.1.1 The folder pipeline

Within a folder the work is a two-stage pipeline joined by a channel:

```
            slices.Chunk(todo, 50)
                     │
        chunks ──────┴──────────────────►  fetch workers   (source lease only)
                                                 │
                                          pending channel
                                                 │
                                           append workers  (destination lease only)
```

`min(srcCap, chunks)` fetchers and `min(dstCap, messages)` appenders, with as
many folders in flight as the source pool is wide — beyond that a folder could
only queue for a connection it cannot have.

**The ordering rule: no goroutine ever holds connections to both accounts, and
where one must be taken while the other is held, the source is taken first.**
Only `prepareFolder` does so, to read the source mailbox before indexing the
destination. Because nothing anywhere acquires a source connection while holding
a destination one, the two pools cannot deadlock against each other however
differently they are sized — including one connection each. That case is a test,
because it is the one a user reaches for when a server starts refusing.

Chunking at 50 sets how finely a folder divides: the number of workers that can
share a folder is the number of chunks in it. It also amortises the one metadata
`FETCH` per chunk over 50 messages, so metadata is one round trip in 51.

A folder that fails does not cancel its siblings; the error is recorded against
that folder and the run continues. Only the caller's context cancels.

### 4.2 Governor — superseded

This section specified a per-side AIMD governor, justified by iCloud throttling
"aggressively and unpredictably" and expected to settle at 2–5 there and 30+ on
mox. Measuring both servers overturned the premise:

| | predicted here | measured 2026-08-28 |
|---|---|---|
| iCloud (source) | throttles, settles 2–5 | **≥48; never refused** |
| mox (destination) | 30+ | **exactly 30, a hard wall** |

The side that pushes back is the destination, and it does so with a stable count
rather than a throttle. The measurement also found a real bug: mox refuses by
hanging up mid-authentication, which `retry.Classify` reads as `unexpected EOF`
and answers with a prompt retry into the same wall.

Superseded by
[the connection governor design](2026-08-28-connection-governor-design.md),
which keeps the decrease, drops the increase, and shrinks to the width that
demonstrably works rather than halving.

### 4.3 Backpressure

A **byte-budget semaphore**, not a count semaphore. A worker acquires
`min(RFC822.SIZE, cap)` bytes before fetching and releases on append completion.

With a count semaphore, 500 concurrent 30 MB fetches is 15 GB of RSS. With a byte
budget, 200 tiny messages and 3 large ones cost proportionally to their actual
size.

The connection pool already bounds concurrency, so it already bounds memory at
`Cap × largest-message`. That is fine at Cap 8 and unacceptable at Cap 100. The
budget exists because a count of connections is the wrong unit for a limit on
memory: messages differ in size by four orders of magnitude.

**Spooling large messages to `os.CreateTemp` is cut.** The design originally
called for messages over about a megabyte to go to disk. With the budget charged
in bytes, total memory is already bounded by the budget; spooling would lower
peak RSS below the budget without changing the guarantee, and would buy that
with temporary-file lifecycle, cleanup after a crash, and disk-full as a new
failure mode. The honest bound is therefore `max(budget, largest single
message)`: a message bigger than the whole budget is charged the whole budget
and read into memory anyway, because refusing to copy it would be worse and
blocking for ever would be worse still. If a mailbox of very large attachments
ever justifies spooling, adding it inside `internal/budget` is contained.

**The message map, not the message bodies, was where the memory went.** The
budget bounds what is in flight, so the obvious next suspect was the other
unbounded read: `FetchFlags` collected a `[]FlagSet` for every message in the
folder, 413,972 of them on the account this is measured against, to find the
three whose flags had moved. It was converted to a streaming callback, and the
peak did not move — 100.5 MB before, 101.1 MB after.

Measuring the phase rather than the change found the real holder. Running the
same folder with `--noresyncflags` came in at 81.5 MB, so the flag resync cost
19 MB; and it cost that in `resyncFlags`, which read the folder's message map
into a slice and then copied it wholesale into a map keyed by source UID. The
folder existed twice over, and neither copy was the thing being streamed.
`Mirrored` now scans straight into the map. Peak RSS 84.5 MB, stable across
runs: 16 MB off a 100 MB run, leaving the flag phase costing 3 MB against a
floor of 81.5.

The streaming change was reverted rather than kept alongside. It was correct and
it was tested, but it bought nothing measurable, and a change that survives on
the argument it was written with rather than the number it produced is how a
codebase fills with plausible work. The lesson is narrower than "measure first",
which was done: the *baseline* has to be the same workload. The first reading
compared against an earlier run that had copied 47 messages, and would have
reported a 9 MB win that was entirely those copies.

The budget is charged *after* the check for a message already at the destination,
not before. Ordering it the other way would make a run that lost its state
database queue and pay for bodies it is about to discard; with the check first,
re-indexing a 414,022-message folder costs a pass over its headers rather than a
pass over its bodies.

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

#### 5.2.1 Measured: what the digest must survive

Implemented in `internal/ident`. Three things were established by running Go's
`net/textproto` rather than by reasoning about RFC 5322:

- **Folding is already normalised for us.** Every fold variant — tab
  continuation, space continuation, trailing space at the fold — parses to the
  same value. What is *not* normalised is an interior whitespace run or a tab
  inside a value, so the digest normalises those itself.
- **A malformed line truncates the header.** `ReadMIMEHeader` stops at the first
  line with no colon and discards every field after it, while still returning
  what it read. Using the partial result is deliberate: a message identified from
  less still gets copied, whereas refusing to identify it makes it uncopyable
  forever. A missing terminating blank line, by contrast, loses nothing.
- **Field framing needs one mechanism, not two.** Values are written in the fixed
  order of `Fields`, each terminated by a byte RFC 5322 forbids in a value, so a
  field is identified by position. Writing the field name as well was redundant
  — each mechanism masked the other's removal under mutation — and was dropped.

`StampHeader` is deliberately *not* in `Fields`: if stamping changed the digest,
the value written would never match the value later searched for.

#### 5.2.2 Bulk adoption versus targeted recovery

Tier 3 has two callers with opposite cost profiles, and conflating them is a
performance bug that only shows up on a real account.

- **Targeted recovery** settles the handful of `in_flight` rows a crash left
  behind: one `SEARCH` each, by stamp or `Message-ID`. Bounded by how many
  appends were in flight when the process died.
- **Bulk adoption** recognises messages already at the destination when the UID
  map cannot help at all. It reads every destination header in the folder once
  and indexes them by digest, so each source message is an in-memory lookup
  rather than a round trip. `SEARCH`-per-message would be 414k round trips on
  iCloud's INBOX (§6.3).

The index is built **only when the folder has never completed a sync, or its
UIDVALIDITY has just changed** — a first sync onto a non-empty destination, a
resumed first sync, or a lost database. Once a folder has completed a sync, the
UID map answers for everything except the suspects, and indexing would read
every header in the folder to learn nothing. Getting this condition wrong is
invisible in tests and ruinous in production.

Two further rules, both of which prevent *losing* mail rather than duplicating
it:

- A digest maps to a **list** of destination UIDs, and adopting consumes one.
  Two identical messages in the source are two messages.
- Destination UIDs already recorded against some other source message are
  excluded from the index. Otherwise a resumed first sync hands one destination
  copy to two source messages and the second is never copied.

Weak identities are never adopted, by either caller. A digest too thin to
distinguish two messages makes a stamp that is too thin as well, so tier 4 does
not rescue them; they are copied, and the duplicate is the accepted cost.

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

### 5.6 CONDSTORE fast path — as built

A folder whose stored `HIGHESTMODSEQ` equals what `SELECT` reports is skipped
outright: no UID listing, no destination connection, one command. On an account
of 144 folders and 776k messages that is most of a repeat run, because the
alternative is enumerating every UID of every mailbox to discover that nothing
moved, against a server charging by the round trip.

**The watermark is a promise, not a timestamp.** It means *everything the source
held at that point is on the destination*. So it is written only by a folder
that finished with nothing failed — `syncFolder` passes zero when
`report.Failed > 0`, and `MarkSynced` treats zero as "no news, keep what you
had". Without that rule a folder that failed one message would be skipped from
then on and that message never retried: a transient failure promoted to a
permanent divergence. The same reasoning governs flag resync, where a rejected
`STORE` counts as a failure for exactly this purpose.

**Modification sequences do not survive a renumbering.** They are only
comparable within one `UIDVALIDITY`, and a renumbered mailbox may report a
*lower* sequence than the one stored — so the assignment is conditional rather
than `MAX(...)`, which would preserve a stale high watermark forever.
`FenceUIDValidity` clears the watermark when it clears the message rows, and the
flag delta asks for everything rather than "what changed since" a sequence that
belongs to a mailbox the server no longer has.

**What the fast path cannot see.** It never selects the destination, so
destination-side drift — a message deleted there, or a `UIDVALIDITY` change at
the far end — is invisible. `--full` is the escape hatch and diffs every folder
regardless. The planned `--reconcile-every` was dropped: it wanted a schema
column and a run counter to cover a case `--full` already covers on demand, and
nothing in the failure reports asked for it.

**Flag resync.** `FETCH 1:* (FLAGS) (CHANGEDSINCE n)` returns only what moved,
so keeping flags in step costs a delta rather than a full scan. Without
CONDSTORE the fallback is the full enumeration, which is what imapsync has
always paid. Writing is `STORE ... FLAGS` — replacement of the whole set, not a
computed add and remove, because the destination is a copy of the source and a
difference applied as two commands is a difference that can be half-applied. The
flags are compared in sorted form: servers are free to list them in any order,
and an unsorted comparison would rewrite every flagged message in the account on
every run and call it work.

The two features compose. An unchanged modification sequence means no flag moved
either, so the fast path skips the resync as well — the cheapest flag sync is the
one that never starts. Default on, matching imapsync's `resyncflags = 1`;
`--noresyncflags` turns it off.

CONDSTORE does not report expunges (that is QRESYNC's job), so new-message
detection still needs a UID diff on any folder that has moved.

### 5.7 Surviving the network

A sync of this account runs for hours against a server nobody controls, so the
question is not whether connections will break but what happens when they do.
Until M3 one dropped connection abandoned a folder. Nothing was lost — state is
written as the copy proceeds — but the run stopped making progress, which over
nine hours amounts to the same thing.

**Failures are classified by what to do about them, not by whether to retry.**
Four outcomes, in `internal/retry`:

| | Meaning | Examples |
| --- | --- | --- |
| `Stop` | The run cannot continue | cancellation, `AUTHENTICATIONFAILED`, `EXPIRED`, DNS `NXDOMAIN` |
| `Skip` | This message, never | `TOOBIG`, `OVERQUOTA`, `NOPERM`, any `BAD`, source message gone |
| `Again` | Transient; reconnect | dropped connection, `EOF`, `ECONNRESET`, read timeout |
| `Slower` | Transient; back off further | `LIMIT`, `INUSE`, `UNAVAILABLE`, `ECONNREFUSED`, "too many simultaneous" |

**Cancellation is checked before anything else.** A cancelled context surfaces
as a read error on whatever connection was in flight, so asking the network
first would classify Ctrl-C as a blip and dutifully reconnect.

**Anything unrecognised is `Skip`.** Guessing "transient" risks a loop that
never ends. Guessing "permanent" costs one warning and a retry on the next run,
because only messages recorded as done are skipped when a run repeats — a
failed message is not a tombstone.

**Backoff is full jitter**: a uniform draw from `[0, base << attempt]`, capped.
A server that drops connections drops all of them at once, and fixed backoff
would reassemble the herd it just scattered.

**A fetch retry resumes at the message that failed**, not at the start of the
chunk. This is correctness, not economy: everything already fetched is recorded
as in flight and may be in the append queue or already on the destination, so
restarting the chunk would copy it twice.

**An append retry asks whether the append landed.** An `APPEND` is complete on
the wire before its response comes back, so a connection lost in between leaves
a message stored and a client that believes otherwise. The retry runs the same
destination search the crash-recovery path uses (§5.2) and records what it
finds instead of sending again. A message whose identity is too weak to search
for is retried regardless: the duplication risk is identical to failing it,
since the next run would re-copy it anyway, but the message actually arrives.

**The ceiling is what makes retrying safe to have.** Retrying assumes something
might still succeed; a destination that has stopped accepting mail fails every
message identically at four attempts apiece, and a large folder would spend
hours establishing that one message at a time. Fifty failures in a row across
the whole run, with nothing copied in between, ends it. The count is
*consecutive*, not cumulative, and the difference is not stylistic: at a fault
rate of one in ten thousand this account would produce seventy-odd scattered
failures, and a cumulative ceiling would kill a healthy run using the very
durability it was given.

**Failed folders get one more attempt at the end.** A folder is usually
abandoned for a reason that has stopped being true by the time the rest of the
run has finished — a server restarting, a mailbox locked by another client, a
burst of throttling — and by then the pools have been rebuilt around fresh
connections. It costs one attempt on the folders that failed, against
rescheduling the entire run to catch them.

**A write that records work already done is not cancellable.** An `APPEND`
finishes on the server before the database hears about it, and an interrupt
lands wherever it lands. Cancelling the write in between leaves the message
copied and unrecorded: the next run rediscovers it by searching the destination,
which is slower, and a message too weak to search for would be copied twice.
That write therefore runs on a context detached from the run's, bounded by its
own ten-second grace so an interrupt cannot make it hang either. Found by
interrupting a real run, which reported `recording message 18 as copied:
context canceled` for a message already sitting on the destination.

**The run says what it is doing.** Not a progress bar: no total is known until
the last folder has been diffed, and against a mailbox holding half the account
the useful question during hour four is not "how far along" but "is it still
moving". A periodic line of copied, adopted, folders finished and rate answers
that, and the counts are the same ones the ceiling watches, so a line that
repeats itself is a run that has stopped.

## 6. Folders

- **`SPECIAL-USE` (RFC 6154) first.** Map by attribute — `\Sent`, `\Trash`,
  `\Drafts`, `\Junk`, `\Archive` — read straight off `LIST`. This is strictly
  more reliable than imapsync's name heuristics and solves iCloud's
  `Sent Messages` / `Deleted Messages` versus mox's `Sent` / `Trash` directly.
- **Name heuristics remain necessary**, contrary to the assumption above.
  Measured against iCloud (§6.3): it returns `\Sent` and `\Trash` while leaving
  `Drafts`, `Junk`, `Archive` and `Notes` unmarked. Attribute mapping therefore
  covers part of an account, and the remainder falls back to names. Note that
  the attributes arrive even though iCloud never advertises SPECIAL-USE, so
  attributes must be read off every `LIST` response regardless of capability.
- **`NAMESPACE` (RFC 2342) next** for hierarchy delimiter (`/` versus `.`) and
  prefix translation. Translate, never assume.
- **User `--map` rules last**, as an explicit override.

### 6.2 Divergences from imapsync

Two deliberate departures, both because imapsync's behaviour is a trap:

**Virtual mailboxes are skipped by default.** imapsync's `possible_special`
table treats `\All` and `\Flagged` like any other special folder, so Gmail's
"All Mail" — which lists every message in the account — gets copied alongside
the real folders it duplicates. `folder.Role.Virtual` marks `\All`, `\Flagged`
and `\Important`; they are skipped with a reason, and `--include-virtual`
restores imapsync's behaviour for anyone who wants it.

**Two source folders resolving to one destination is a hard error.** Merging is
not recoverable and not detectable afterwards: the destination simply has more
messages, and every subsequent run's diff looks like an ordinary first sync. A
source with both `Sent` and a stale `Sent Messages` hits this against any
destination with a `\Sent` folder. `folder.Build` refuses the whole plan and
names the colliding folders.

A third, smaller one: a folder whose *name* contains the destination's hierarchy
delimiter is skipped rather than written out, since writing it splits one
mailbox into two levels of a tree nobody asked for. No encoding avoids this, so
the choice is between skipping loudly and restructuring silently.

### 6.3 Measured: iCloud, 2026-08-27

`probe` against a real account, 144 folders and 776,747 messages. This replaces
several guesses the design rested on.

| Capability | iCloud | Consequence |
|---|---|---|
| IMAP4rev1 | yes | no rev2; nothing may be assumed implied |
| CONDSTORE | **yes** | the §5.6 fast path is available on the throttled side |
| QRESYNC | **yes** | supported by the server, unimplemented by go-imap/v2 |
| UIDPLUS | yes | `APPENDUID` works, so identity tier 2 is exact |
| LIST-STATUS | advertised | **unusable**: no LIST-EXTENDED, so no return options |
| LIST-EXTENDED | no | plain `LIST` only |
| SPECIAL-USE | no | yet `\Sent` and `\Trash` still appear in `LIST` |
| MOVE | no | no cheap moves |
| LITERAL+ | no | has `XAPPLELITERAL`; a round trip per append |
| MULTIAPPEND | no | one `APPEND` per message |

Three things follow:

**QRESYNC is not worth implementing ourselves — measured, not assumed.** It was
deferred post-v1 on the grounds that go-imap lacks it, and §10 asked for the
cost of a full resync on the 414,022-message folder before reconsidering. That
number now exists. A `--full` dry run over INBOX alone examined 413,954
messages in **51.2 s**; a `--full` run over the whole account settled 776,802
messages across 141 folders in **1 m 27 s**, 137 of them inside the first 30
seconds.

That is the worst case QRESYNC exists to remove, and it is under a minute and a
half for three quarters of a million messages. The expensive part of a resync
turned out to be neither the UID list nor the round trips but nothing at all:
intra-folder chunking across a pool already hides it. Writing an extension that
go-imap does not have, against the only server we know that advertises it,
to save seconds on a run measured in hours of copying, is not a trade worth
making. Revisit only if a folder appears whose reconcile is itself slow.

**Folder enumeration costs 144 round trips — but only in `probe`.** Parallelising
those `STATUS` calls was carried as an outstanding item from M2 to M4 and then
cut, on discovering that `sync` never asks for `STATUS` at all: it lists folders
with no return options and learns the counts it needs from `SELECT`, one folder
at a time, already spread across the pool. The serial loop lives on a single bare
connection inside `probe --status`, a diagnostic run occasionally. Giving `probe`
a pool to speed itself up — against an account with strict connection limits —
buys nothing the sync pays for. It can be built when something real asks for it.

**Folder enumeration costs 144 round trips.** LIST-STATUS being unusable means a
`STATUS` per folder, 60–200 ms each, which is most of the 35.7 s probe. This is
a natural first consumer of the destination pool: the calls are independent and
parallelise across connections. Note that `imapx` deliberately does *not* try
`RETURN (STATUS)` on the strength of the LIST-STATUS claim alone — on the one
server known to advertise it that way it is guaranteed to fail, so the retry
would be a wasted round trip and a spurious warning on every run.

**INBOX holds 414,022 messages, 53% of the account.** Per-folder parallelism
alone would leave the run bounded by one folder on one connection. Intra-folder
UID-range chunking is not an optimisation for this account; it is the whole job.

Connection ceiling: at least 8, the configured cap; iCloud did not refuse. The
real limit is still unmeasured.

### 6.4 Measured: iCloud → mox, first real sync, 2026-08-27

135 messages across two folders (`AU`, 8; `Laust`, 127), iCloud to mox over one
connection per side.

| Run | Result | Wall clock |
| --- | --- | --- |
| First | 135 copied | **1 m 17 s** |
| Second, state intact | 0 copied, 135 already | 0.9 s |
| Third, state database deleted | **0 copied, 135 adopted** | 4.2 s |

Three things this settles.

**Throughput is 1.7 messages/second.** Extrapolated to the measured 776,747
messages, a single-connection run of this account takes **5.3 days**. M2 is not
a refinement; without it the tool does not work at this size. The number also
gives M3's governor something to beat.

**mox does not rewrite messages on APPEND.** The third run recomputed a digest
from what mox had stored and matched all 135 against the source. Had mox added
or reordered headers, adoption would have silently failed and a lost state
database would re-copy the whole account. The identity ladder (§5.2) survives at
least one real destination; Dovecot and Gmail are still unproven.

**Bulk adoption costs a folder scan, not a re-copy.** 135 headers took 2 s. At
414,022 that is a real cost but a survivable one, and it is paid only in the
cases §5.2.2 lists.

Two things this did *not* settle: no message in the sample produced a weak
identity (135 of 135 indexed), so tier 4 stamping is still only proven in
tests, and no run has been interrupted mid-append against a real server.

**mox's certificate is self-signed with a ten-year validity**, which macOS
rejects outright as `not standards compliant` — keychain trust does not help.
This is why TLS verification is per side (`--source-insecure`, `--dest-insecure`)
rather than one flag: reaching a self-signed destination on your own network
must not also stop verifying a public source over the internet. Certificate
pinning would be better than either and is not built.

### 6.5 Measured: concurrency, iCloud → mox, 2026-08-27

Same accounts and same code path as §6.4, with the M2 engine.

| Run | Folders | Messages | Source × dest conns | Elapsed | Rate |
|---|---|---|---|---|---|
| M1 baseline | 2 | 135 | 1 × 1 | 1m17s | 1.7/s |
| M2 | 2 | 135 | 4 × 8 | 26.7s | 5.1/s |
| M2 | 2 | 135 | 8 × 16 | 24.2s | 5.6/s |
| M2 | 14 | 10,730 | 8 × 16 | 7m55s | **22.6/s** |

**13× the baseline**, and no failures at eight concurrent iCloud connections.
Extrapolated over the 776,747-message account, 5.3 days becomes about 9.5 hours.

The two-folder runs are the more interesting measurement, because doubling the
pools bought almost nothing. Both folders were small — 127 messages and 8 — and
at a chunk size of 50 a 127-message folder splits into three chunks, so at most
three connections can ever share it. Pool size cannot help past that.

That ceiling was left in place rather than fixed. Dividing each folder by the
pool width instead of by a fixed 50 would lift it, but the 14-folder run shows
folder-level concurrency already takes up the slack whenever there are folders to
spare, and the target account has 144 of them. The case where it would bite —
few folders left, all of them small — is by construction the cheap part of a run.
Worth revisiting only if a measurement, rather than an argument, calls for it.

## 7. Safety

Destructive operations (`--delete2`, `--delete1`, expunge) require explicit
opt-in **and** pass a safety valve: a run that would delete more than a threshold
(default 10%) of a folder refuses unless `--force` is given.

### 7.1 What `--delete2` deletes — as built

imapsync's semantics: the destination ends up looking like the source, including
losing mail that arrived by some other route. A narrower version was built first
— only messages this tool had copied and recorded — and rejected, on the
grounds that somebody moving over from imapsync would find their destination
quietly accumulating everything the tool declined to take responsibility for.
A surprise that arrives silently and only shows up as a mailbox that never quite
matches is worse than one that deletes.

Two populations, one question:

- **Messages we copied** are checked against the source's UID listing, which is
  exact.
- **Everything else on the destination** is checked by identity, the same digest
  adoption matches on, against the identities the source currently holds.

The source's identity set is read from the state database rather than from the
source: every message the source holds has been identified once already, and
writing that down was the cheaper half of doing it. Rows whose UID the source no
longer lists are excluded — that message is gone, and its identity going with it
is the entire point. Getting that filter wrong leaves a second copy of a deleted
message behind for ever, which is what §9.7 records.

**The one place this stops short of imapsync, deliberately.** A message carrying
too little header to identify is left alone. Adoption already refuses to match
on a weak digest because a wrong match drops mail; deleting on one is the same
mistake with no way back, since "the source has nothing like this" cannot be
concluded from a digest that could not have recognised it either way.

**Cost falls where it should.** On a mirror this tool built, every destination
message is claimed by a row, so the pass is one UID listing and no header reads.
Only mail that arrived another way is paid for, and only once — after it is
deleted or adopted there is nothing left to ask about. Measured against mox: a
steady-state `--delete2` run over a 59-message folder took 19ms.

**UIDPLUS is required, not preferred.** Plain `EXPUNGE` is defined to purge
every `\Deleted` message in the mailbox, including ones the account owner
flagged by hand in their own client. `DeleteMessages` therefore returns
`ErrNoUIDExpunge` rather than falling back to it. A mutation swapping
`UID EXPUNGE` for `EXPUNGE` confirmed this empirically by destroying a
bystander's message.

### 7.2 The valve has a floor as well as a ceiling

The denominator is the destination folder, because the destination folder is
what is at stake once mail nobody recorded is in scope. The failure being
guarded against is a source that answers a UID listing with nothing or a
fraction of the truth, and that arrives as a large share of the destination
being nominated at once.

A useful consequence: the first `--delete2` run against a destination holding
mail of its own will usually refuse, which is the right way to find out. Verified
against mox — a destination seeded with 12 foreign messages refused at 12 of 71,
16.9%, exit status 1, and `--force` then produced an exact 59-to-59 mirror.

A proportion alone is not enough. One message out of six is 16.7% and would trip
a 10% ceiling, but that is the most ordinary thing that happens to mail. A guard
that fires on the ordinary case gets `--force` written into the cron line
permanently, and then it is not a guard. So a small absolute number of deletions
(10) is always allowed. A source that lies about a six-message folder costs six
messages; the disaster worth stopping is the one that lies about a
four-hundred-thousand-message folder, and that is nowhere near the floor.

This was found by a test, not by reasoning: the first CLI test of the happy path
refused to delete a single message from a six-message folder.

### 7.3 Ordering and barriers

`ForgetMessages` runs only *after* `DeleteMessages` succeeds, on a
`context.WithoutCancel` timeout. A row removed before a failed expunge would
leave a message on the destination that nothing remembers putting there, and so
nothing would ever remove.

Deletion is skipped for a folder that failed to copy anything (its picture of
the source is already known to be incomplete) and for a fast-pathed folder.

Both the real run and the dry run go through `condemned()`. A preview computed
by a second implementation previews that implementation. The dry run costs a
destination connection it would not otherwise need, and selects read-only,
because the destination UIDVALIDITY is half the question: if the destination was
renumbered the real run finds no message map and deletes nothing, so the preview
must agree.

A refusal is not a failure — nothing went wrong and nothing was lost — but it
exits non-zero, because a zero exit would let a scheduled sync report success
for months while a folder quietly stopped being mirrored.

### 7.4 Copying and deleting are watermarked separately

Found by verifying against real servers, not by tests. The stored modseq meant
one thing — "everything the source held at that point is on the destination" —
which is a statement about copying only. A run without `--delete2` advances it
while carrying out no deletions at all, so the next run *with* `--delete2` found
the folder unchanged, skipped it, and lost those deletions permanently. Only
`--full` recovered them.

That fires on the most natural way to adopt the flag: sync for months, then add
`--delete2`. Every folder's watermark is current, so nothing is ever deleted.

Folders therefore carry `src_deleted_through` alongside `src_highestmodseq`, and
the fast path may skip a folder only if deletions have been carried out through
the same point. A refused deletion does not advance it either — otherwise the
safety valve would turn into the failure it exists to catch, the folder looking
settled and never being offered again.

This needed the first schema migration. `CREATE TABLE IF NOT EXISTS` does
nothing to a table that already exists, and an existing state database is
exactly the one with something to lose, so `Open` now applies `ALTER TABLE`
statements and treats "duplicate column name" as success.

### 7.5 The fast path does not hide deletions

Checked rather than assumed: RFC 7162 requires any server storing modsequences
to increment HIGHESTMODSEQ when at least one message is permanently removed. A
source expunge therefore wakes the folder, and `--delete2` needs no `--full`
implication. iCloud advertises QRESYNC, confirmed with our own `probe`.

## 8. Surface

### Commands

- `sync` — native interface.
- `compat` — imapsync flag translator. See §8.1.
- `probe` — connect, dump capabilities, measure the connection ceiling, print a
  suggested config. Particularly useful against iCloud.
- `status` — read the state DB.
- `verify` — standalone consistency check.

### 8.1 The compat shim — as built

Someone with a working imapsync line has already solved the hard part. The shim
exists so that line keeps working, and so the migration to native flags is a
thing they can do later rather than a precondition.

**Invocation.** `imapsync-go compat <imapsync flags>`, or by name: if `argv[0]`
is `imapsync`, the argument list is treated as a compat invocation. A symlink
therefore makes the binary a drop-in for scripts that call `imapsync` directly
and have no subcommand to hang the translation off.

**Getopt::Long semantics were verified, not remembered.** A Perl script
declaring all 218 of imapsync's options was run over a corpus of spellings and
its answers recorded in `internal/compat/getoptlong_test.go`. Two of them
contradicted what the implementation would otherwise have assumed:

- `--dry=0` is an **error**, not a negation. A `!` option never takes `=value`.
- `--auto` resolves to `automap`, while `--expung` is ambiguous between four
  options despite being longer. Whether an abbreviation is ambiguous is a
  property of the entire table, so it cannot be reasoned about one option at a
  time and the answer changes as the table does. This is exactly the kind of
  thing a golden file should hold and a person should not.

Also confirmed there: matching is case-insensitive, a single dash is accepted,
a mandatory value swallows the next argument even when it looks like an option,
negated forms take part in abbreviation, and `--noexclude` is its own option
rather than a negation of `--exclude`.

**Four dispositions, with `refuse` as the zero value** — an option someone
forgets to classify refuses rather than silently doing nothing:

| | |
|---|---|
| `refuse` | Changes which messages move or what becomes of them, and cannot be honoured exactly. Also every unknown option. |
| `translate` | Has a native equivalent. |
| `ignore` | Asks for what already happens, or cannot mean anything here. **Always reported with a reason.** |
| `endpoint` | Folded into `--source-url`/`--dest-url`. |

The failure mode being designed against is silence. Accepting `--maxage 30` and
not applying it copies twelve years of mail instead of a month, and the user
finds out from a full disk. Refusing it costs them one edit.

Refusals are collected and reported **together**. Discovering four unsupported
flags one run at a time is four round trips through a cron table.

**Dispositions are per-polarity**, because for several options the two
directions are not equally safe:

- `--syncinternaldates` is ignored (it is what happens); `--nosyncinternaldates`
  is refused.
- `--sslcheck` is ignored; `--nosslcheck` translates.
- `--uidexpunge2` is ignored; `--nouidexpunge2` is refused, because plain
  EXPUNGE would purge messages the owner flagged by hand.

**Passwords never become arguments.** `--password1` goes into the environment
and the translation names the variable. Arguments are readable by every process
on the machine, and the translation is printed for pasting into bug reports.

**The printed translation is the thing that runs.** `Plan.Args` is both echoed
and executed, so the two cannot drift.

**One assumption, stated.** imapsync probes port 993 when told neither `--ssl1`
nor `--tls1` (imapsync.pl:21870ff). Replicating that would make translation
depend on a network call. The shim assumes TLS and prints an `Assumed:` line.
It never assumes in the other direction.

**Provider bundles were read rather than guessed.** `--office1` is exactly
`--host1 outlook.office365.com --ssl1 --exclude "^Files$"`, so it is translated
in full. `--exchange1` does literally nothing in imapsync, so it is ignored
with that as the stated reason. `--gmail1` sets rate limits, folder rewriting,
label sync and folder ordering, so it is refused.

**The coverage test is the contract.** `imapsyncOptions` holds all 218 specs
verbatim from imapsync's two `GetOptions` calls; one test asserts the table
covers every one with matching kind and aliases, another that it invents none.
A new imapsync release is a diff against that list.

**`NativeFlags()` exists for a test in the command package**, which checks every
flag the table emits against the flags `sync` accepts. A typo in a native flag
name is otherwise invisible: the translation prints, looks right, and dies at
the far end naming a flag the user never typed.

**Dropped:** an `action` disposition for `--justconnect`/`--justlogin`/
`--justbanner`. `probe` takes one endpoint at a time, so honouring them meant
two invocations for three flags nobody puts in a cron line. They are refused
with a pointer to `probe`.

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
3. **Fault injection.** M3 does this with decorators wrapped around the
   in-process server rather than a proxy in front of a real one: they inject
   the same faults — a connection dropped mid-fetch, an append that succeeds on
   the wire and fails on the way back, a server that refuses everything — and
   they do it deterministically, on a chosen message, without a second process.
   A proxy would add a class of fault a decorator cannot reach, the stalled
   literal that is neither an error nor a completion, and it can be built when
   something needs it. What must not happen is a resumability bug that cannot
   be reproduced on demand, and the decorators reproduce these on demand.

**Core property test:** *sync twice, assert the second run copies zero messages.*
That single invariant is the tombstone for the duplication bug.

### 9.1 Mutation testing, and what it found in M2

Every component is checked by breaking it deliberately and confirming a test
fails. Three M2 mutations survived, and each was a test that was weaker than it
looked rather than a mutation that did not matter:

1. **An unlocked adoption index survived** an end-to-end test of 240 concurrent
   adoptions, five times over, with no race report. Each adoption brackets a
   map access of some tens of nanoseconds between two SQLite writes of about a
   millisecond, so the window is real but vanishingly narrow. Covered instead by
   a test that calls the index directly from eight goroutines and asserts each
   destination message is handed out exactly once.
2. **Deleting the loop that reclaims budget from a failed folder survived.**
   Instrumentation showed exactly two messages stranded per failure: the
   surviving append workers drain most of the channel themselves. Two messages
   will not starve a run, so the invariant is now asserted directly — after the
   run, the whole budget must be acquirable.
3. **A test named the wrong mechanism.** It claimed to prove the source is
   opened read-only, but fetches use `BODY.PEEK`, so they never set `\Seen`
   whichever way the mailbox was opened. Renamed to state what it actually
   proves, with the `PEEK` flag as the mutation target.

A fourth mutation — making the fetch-to-append channel unbuffered — survives and
is left alone: the buffer smooths jitter and its absence is only slower.

Two tests assert concurrency itself, since every other test here would pass on a
strictly sequential engine. They hold body fetches, and separately appends, at a
rendezvous that only completes once four are in flight at once. An earlier
version asserted elapsed time instead and failed inside the full suite while
passing alone: a wall-clock test measures the machine as much as the code.

### 9.2 What M3's mutations found

Fifteen mutations of the retry paths, of which two survived and both were
informative.

**The guard on the second pass survived.** Removing `ctx.Err() == nil` from the
condition that decides whether to re-attempt failed folders changed nothing any
test could see, because by the time a run is cancelled every folder's error
wraps `context.Canceled` and is filtered out anyway, and because `pass` refuses
to start work on a cancelled context. The clause is kept for the one thing it
still does — stop the run announcing a retry it will not perform — and is
recorded here as scaffolding rather than structure.

**`--give-up-after` and `--attempts` were cut.** Removing their wiring survived,
because no test could tell whether the flags reached the engine: the CLI tests
run against a real in-process server with no way to make it fail on demand.
Rather than build a seam for a knob nobody has asked for, the flags were
removed. The engine's defaults are the ones §5.7 argues for, and a flag can be
added when a real run shows it is needed — with a test, by then, of what it
does.

### 9.3 What the real servers found that the tests did not

Two defects survived a green suite and appeared within minutes of interrupting a
run against iCloud and mox:

1. **A cancelled context abandoned the write that records a completed copy**,
   which is the exact opposite of write-ahead state (§5.7). Both the fix and a
   test now exist; the test asserts that a second run adopts nothing, since
   adoption is the signature of a copy the database failed to hear about.
2. **An interrupted run printed no report at all.** It had copied 47 of 127
   messages and said only that the context was cancelled. The report is now
   written before the error is returned, because what a stopped run managed to
   copy is the thing worth knowing: it is what the next run will not repeat.

Neither is exotic, and neither would have been found by more unit tests. The
first needs an interrupt to arrive inside a window a few hundred microseconds
wide, which a real network makes routine and a loopback server makes rare.

Of the thirteen caught, the one worth naming is removing the check that asks
whether an append landed before retrying it. It produces silent duplicates, on
exactly the failure the retry exists to survive, and only the exactly-once
assertion catches it.

### 9.4 What M4's mutations found

Fifteen mutations across the fast path, the flag resync, the state layer and the
command line. Three survived; two of them were real gaps and one is documented
scaffolding.

**A rejected `STORE` was not counted as a failure.** Removing `lv.failed` from
the flag path changed nothing any test could see, because no test made the
destination refuse a `STORE`. The consequence would have been the tombstone trap
in flag-shaped form: the folder finishes clean, the watermark advances, the fast
path skips the folder from then on, and one transient rejection becomes a
permanent divergence. A decorator that refuses on demand now proves the folder is
looked at again.

**Flags were stored and never remembered.** Dropping the `RecordFlags` write
survived, because the tests that counted `STORE`s never changed a flag twice.
Every later run would have found the stored flags still disagreeing and issued
the same command again — harmless once, a permanent tax on any folder somebody
keeps touching. The test now runs a third pass over a folder that still looks
changed and asserts nothing more is written.

**A stale watermark survived a renumbering.** Removing the clause that clears it
changed nothing, because the in-process server does not implement CONDSTORE and
ignores `CHANGEDSINCE` — so a delta asked for relative to a meaningless sequence
still returned everything. Against a real server it would have returned nothing,
and the flags the fence had just made the state forget would have stayed
forgotten. The test asserts on the sequence the fetch was asked for rather than
on its result, which is the only observable the fake server can offer.

**The barrier on the skipped folder is scaffolding.** Removing the `p.skipped`
check survived, because a folder the fast path recognised has no destination
mailbox recorded, so the flag resync finds no mapping to work through and stops
anyway. It is kept and recorded here for what it prevents rather than what it
does: a later change to that query turning a skipped folder into `STORE`s issued
on a connection that never selected the mailbox.

**Two tests exist only because a mutation demanded them.** The in-process server
reports no `HIGHESTMODSEQ` at all, so everything about the fast path is tested
through a decorator that states the sequence the test wants — including the case
a real server makes hard to arrange, where the sequence has genuinely not moved.

### 9.5 The UIDs iCloud lists but does not have

Verifying the fast path on a genuinely large folder turned up something the
in-process server could never have shown. A `--dry-run` over iCloud's INBOX
reported **503,786 messages to copy** from a mailbox whose `STATUS` says it holds
**414,053**.

The measurement was repeated in raw Python `imaplib`, so it is not a bug in this
tool or in go-imap:

| Question | Answer |
| --- | --- |
| `STATUS INBOX (MESSAGES)` | 414,053 |
| `EXISTS` on `SELECT` | 414,053 |
| `UID SEARCH ALL` | **503,786 distinct UIDs** |
| Highest UID returned | 570,679 (`UIDNEXT` 570,688) |
| `UID FETCH` of the first 500 searched UIDs | **358 returned data** |

So `SEARCH` over-reports by about 18%, and the surplus UIDs are not messages
that were deleted while we looked: fetching them succeeds and returns nothing.

**Why the tests were quiet.** `FetchMeta` builds its result by iterating what the
server sent back, and `fetchChunk` iterates the metadata rather than the chunk it
asked for, so a UID with no message simply fell out of the pipeline. It was never
copied, never failed, and never recorded. The run reported `to_copy 503786`,
`copied 414053`, `failed 0` — which reads like ninety thousand lost messages and
was in fact ninety thousand numbers that never existed.

Nothing was lost or duplicated, but the silence cost two things. The report was
unreadable, and because nothing was written down, every later run in which the
INBOX had changed re-enumerated all 89,733 of them and spent about 180 chunk
round trips asking a server that charges by the round trip.

**The rule adopted: the source not having a message is a fact, not a failure.**
A failure returns to the queue on the next run and holds the folder's watermark
down, which is right for something that could not be copied and wrong for
something that does not exist. `state = 'gone'` records it instead. Within one
UIDVALIDITY a UID is never reissued, so the record is permanently safe, and
`FenceUIDValidity` discards it along with everything else when the mailbox is
renumbered.

**It also fixes a bug that predates the discovery.** A message expunged between
enumeration and fetch took the `ErrMessageGone` path, whose comment said it
recorded a failure "so the next run does not retry forever" — which is precisely
what recording a failure fails to do. A single such message would be re-queued
and re-failed on every run, and because a folder with failures keeps its old
watermark, it would deny that folder the CONDSTORE fast path permanently. Both
sites now record the message as gone.

**The interpretation this rests on.** A `UID FETCH` that completes and says
nothing about a UID is the server saying there is no such message; a fetch that
went wrong returns an error and never reaches the recording. The residual risk is
a server that transiently omits a real message from an otherwise successful
response, which would leave that message permanently skipped. Accepted: the only
alternative is a verification round trip per UID, which at 89,733 UIDs costs more
than the problem.

**What a mutation caught that a test did not.** Sparing gone rows from the
UIDVALIDITY fence initially survived, because the test asked for the surviving
rows under the *new* numbering — where they could not appear whether they had
been deleted or not. Every read is scoped by UIDVALIDITY, so the assertion had to
be made under the old one to mean anything.

### 9.6 What mutation testing found in `--delete2`

Eighteen mutations, sixteen caught. The two survivors are documented barriers,
not gaps: removing the `p.skipped` guard from `deleteVanished` changes nothing
observable, because a fast-pathed folder has a zero destination mailbox and so
an empty message map anyway. It is kept against a later change that populates
either earlier, which would turn "the server says nothing has changed" into a
folder full of deletions.

**A test that passed for the wrong reason.** Dropping the `!s.opts.Delete2`
guard survived: with deletion off, the source listing is nil, so *everything*
looked doomed — and the ceiling refused it. The test asserted `Deleted == 0` and
passed. Fixed by also asserting `Refused == 0`: "off" has to mean the machinery
never ran, not that a second guard caught it.

**A harness that could not fail.** Comparing `DstUID` instead of `SrcUID`
against the source listing survived because the fixture gave both sides
identical UID numbering. That mutation deletes *arbitrary innocent messages* on
a real server. The harness now stuffs five throwaway messages into the
destination and expunges them, so the two sides number differently and the
confusion is visible.

**A fixture that proved the opposite of its name.** The test that deletion never
touches mail it did not put there seeded the destination using the helper that
names messages after the mailbox — producing "strangers" byte-identical to
source messages, which were duly adopted, and then correctly deleted. Genuine
strangers need genuinely different content.

**A destructive query nothing pinned.** `ForgetMessages` ignoring its
UIDVALIDITY argument survived, because fencing means two numberings never
coexist in practice. Asserted anyway: the statement is a `DELETE`, and the cost
of discovering later that it was scoped too widely is measured in other people's
mail.

### 9.7 What mutation testing found in the widening

**The filter that only matters where the two paths cross.** Judging strangers
against every identity ever recorded, rather than only the ones the source still
holds, survived every test. It shows up in one situation: a destination with two
copies of a message, one recorded and one not, when the source deletes the
original. The recorded copy goes because its UID is no longer listed; the
unrecorded copy matches a row describing a message that no longer exists and
stays behind for ever. Tested now.

**A fixture that tested the wrong thing twice.** The test that a second copy of
something the source still holds is kept was written with the same subject but a
different Message-ID — a different message, correctly deleted. It only means
anything with a genuine twin, the same bytes on both sides.

**The denominator needed its own test.** Nothing distinguished "a share of the
destination folder" from "a share of what we copied" until a folder was given
mail from elsewhere and a ceiling set between the two fractions.

Two survivors are barriers. Skipping the header read when nothing is unclaimed
is an optimisation with no behaviour to observe, and selecting the destination
read-only in a dry run cannot be caught by a test that never writes.
See also §9.6, whose findings still stand.

### 9.8 What mutation testing found in the compat shim

Sixteen mutations, sixteen caught — the highest rate in the project so far,
which says more about the shape of the problem than about the tests. A
translator is close to a pure function: an argument list in, an argument list
out, with no server, no clock and no concurrency in the way. Almost every
mutation changes an output somebody is already asserting on.

The two worth recording are the ones aimed at the failure modes the design is
built around:

**A password onto the command line.** Making `side()` emit
`--source-password` with the value instead of `--source-password-env` with a
variable name is a one-word change that produces a translation which works
perfectly and leaks the password to every process on the machine. Caught twice:
once by a test that scans both the arguments and the printed translation for
the password, and once by the end-to-end translation test.

**A typo in a native flag name.** `--log-lvel` instead of `--log-level`
survives compilation, survives every test in the compat package, prints without
complaint, and fails at the far end naming a flag the user never typed and
cannot find in any documentation. Nothing in the package can catch it, because
the package cannot see the flags it is translating into. `NativeFlags()` and a
test in the command package exist for this one mutation.

No survivors, and no barriers — which is itself the argument for keeping the
translator free of I/O. The TLS assumption (§8.1) is what that buys: a shim
that probed port 993 to decide would have a network call in the middle of a
pure function, and the mutations that matter would stop being observable.

## 10. Milestones

- **M0** — skeleton, config, `probe`, capability negotiation. *Done.*
- **M1** — single-connection correct one-way sync + SQLite state. *Done.*
- **M2** — pools, staged pipeline, byte budget. *Done.* Spooling was cut (§4.3).
- **M3** — resilience: retry with backoff, a run-wide failure ceiling, a second
  pass over failed folders, progress reporting (§5.7). *Done.* The AIMD governor
  was deferred: the throttling it exists to answer had not been observed, and a
  controller tuned against a server that is not pushing back is a controller
  tuned against nothing. Deferring it was right — when a wall was finally
  measured it was on the destination, not the source, and it was a hard
  connection count rather than a throttle, so §4.2's controller would have been
  built against the wrong thing. A governor was built afterwards — shrink-only at
  first, and growing again once a moving ceiling was measured; see
  [its design](2026-08-28-connection-governor-design.md).
- **M4** — CONDSTORE fast path, flag sync, `SPECIAL-USE` mapping with name
  fallback (§6). *Done.* `SPECIAL-USE` mapping was already built in M1;
  `--reconcile-every` was dropped in favour of `--full` (§5.6); parallel folder
  `STATUS` was cut, because it is a `probe` cost and not a sync one — see below.
- **M5** — `compat` shim (§8.1, §9.8), `--delete2` + safety valve
  (§7.1–7.4, §9.6). *Done.* Progress UI dropped: M3 already logs progress and a
  rate.
- **Post-v1** — MULTIAPPEND batching, COMPRESS, and broader server support:
  Dovecot, Gmail, Microsoft 365, Cyrus. QRESYNC was **dropped**, not deferred;
  see below.

QRESYNC was the least settled part of this plan, and it is now settled against.
It was placed post-v1 because go-imap lacks it, then argued back up on
discovering iCloud advertises it (§6.3), with the deciding question left as:
what does a resync actually cost on a 414,022-message folder? Measured, it is
**51.2 s** for that folder and **1 m 27 s** for all 776,802 messages in the
account. Intra-folder chunking across a pool already removed the cost QRESYNC
targets. Implementing an extension the library does not have, to save seconds
on runs measured in hours, is not worth it. See §6.3.

M1 precedes M2 deliberately. Concurrency bugs layered on an unproven diff are
undebuggable.
