# Local store — backup to disk and restore from it

Status: proposed. Nothing here is built.

## 1. Purpose

Sync an IMAP account to a directory on disk, and back again.

This is the first feature in this project with **no imapsync counterpart**.
imapsync's author is unambiguous — issue #240 and `FAQ.Local_Storage.txt`:

> R1. No. Imapsync plays with IMAP servers only, as an IMAP client, over a
> TCP/IP connexion.

He points people at isync/mbsync or Rick Sanders' tools instead. So there is no
option to imitate and no drop-in obligation: `compat` gains nothing here, and
the flag names are ours to choose. That is a freedom, but it also means this
feature cannot hide behind "imapsync does it this way".

The same FAQ carries the sentence that should shape the whole design:

> Remember that doing backups without trying the restore process is more
> dangerous than doing no backups at all. No backups makes people act in a
> safer way.

A backup format this tool can write but not read would be worse than nothing.
**Restore is in scope from the first milestone**, not deferred.

## 2. Format: one file per message, not mbox

The request arrived with an example: two folders exported from Mail.app to
`~/Downloads/mail`, as `INBOX.mbox/` and `Laust.mbox/`, each an Apple package
holding an `mbox` file and a `table_of_contents`. It is a reasonable thing to
imagine, and it is the wrong target — because it is an *export*, and this needs
a *mirror*.

The example makes the argument itself. `INBOX.mbox/mbox` is **one file of
2,147,494,267 bytes**. Four consequences:

- **Deleting one message means rewriting 2.1 GB.** Every incremental run that
  reflects a deletion pays the size of the folder.
- **Nothing to address a message by.** Messages are concatenated; there is no
  identity to map a UID onto except a byte offset, which every rewrite moves.
- **One writer.** mbox needs a lock over the whole file, so a tool whose entire
  premise is 16–40 workers in parallel would collapse to one. The concurrency
  that makes this project worth having would be switched off by its own backup
  format.
- **The `From ` ambiguity.** A body line beginning `From ` must be escaped, and
  the variants disagree about how (mboxo, mboxrd, mboxcl, mboxcl2). Round
  tripping through the wrong pair corrupts message bodies. It is also why
  counting `^From ` in that file gives an estimate rather than a number.

Maildir answers all four, because it was designed for exactly this. One file
per message: parallel writes with no lock, delete is one `unlink`, and the
message never moves. Identity lives in the filename. Delivery is `tmp/` →
`fsync` → `rename` into place, and POSIX rename is atomic — which makes a local
APPEND *safer* than the IMAP one, since a half-written message cannot be
observed.

One file per message is also what makes this cheap to back up in turn. An
incremental backup of a 2.1 GB mbox re-reads 2.1 GB to discover that one
message arrived. A directory of individual messages hands the backup tool
exactly the changed set, and nothing else.

The count in the example is itself the lesson about exports. The source INBOX
holds 413,954 messages; `INBOX.mbox` holds roughly 58,694. Mail.app wrote out
what it had cached. An export is a snapshot of a client's opinion; a mirror is
a statement about the account.

An mbox *writer* remains reasonable later as a one-way export for feeding other
tools. It is not the sync target.

### 2.1 Maildir's mechanics, not its filenames

Messages are named `<uid>.eml`, and that decision is worth stating plainly:
**this is not a maildir, and no maildir-aware tool will read it.**

The requirement is that a message can be opened by double-clicking it. `.eml`
is what macOS and Windows both recognise as a mail message, so Mail.app and
Outlook open one directly. Maildir cannot spell that. Its grammar is
`<unique>:2,<flags>`, where everything after the colon is flags — so `.eml`
placed last is parsed as four bogus flags, and placed anywhere else it is not
an extension. The colon is its own problem: Finder renders `:` as `/`, so
maildir names are actively misleading in the one interface this is meant to
serve.

That compatibility is not wanted. The store is a waypoint between IMAP servers,
not a format for other mail tools to read: what it has to do is hand every
message back to an arbitrary IMAP server intact. Being legible to mutt or
mbsync was never the goal, and §8's second decision had already mirrored the
IMAP hierarchy as ordinary nested directories rather than Maildir++, which
mbsync cannot read either. Half-honouring a convention nothing can use is worse
than declaring the format, so:

```
Archive/                      a folder
Archive/.imapsync-folder.db   flags, dates, UID bookkeeping; hidden
Archive/.tmp/                 staging, same filesystem, for atomic rename
Archive/0000009083.eml        a message
Archive/2024/                 a subfolder
```

A directory is a folder; a `.eml` is a message. Opening `Archive` in Finder
shows the mail and nothing else. UIDs are zero-padded to ten digits so that
lexical order — which is what every file browser and `ls` shows — is UID order.

Kept from maildir: one file per message, write-`fsync`-rename delivery, delete
by `unlink`, and no lock anywhere on the message path. Dropped: `cur/new/tmp`,
the colon, and flags in the filename.

### 2.2 Message files are immutable

Flags do not go in the filename. They go in a SQLite database in the folder,
`.imapsync-folder.db`.

Maildir changes flags by renaming, which means a message read once is a
different filename for ever after. For a store whose point is to be backed up
incrementally that is the wrong trade: `\Seen` sweeping a folder would rename
thousands of files, and a backup tool without rename detection would copy every
one of them again. Content-addressed tools survive it; `rsync` does not.

With flags in a database, **a message file is written exactly once and never
touched again.** That is the property that makes an incremental backup of this
store proportional to the mail that actually arrived.

SQLite rather than a JSON document, and the reason is the argument of §2 one
level up: a JSON index has to be re-serialised in full to change one entry, so
marking a single message read would rewrite all 413,954 entries — the mbox
problem again, in the file that was supposed to avoid it. SQLite updates one
row, and the project already runs it under forty concurrent writers in
`internal/state`, including the WAL settings and the lessons about SQLITE_BUSY.

It costs a lock the filename approach did not need, and that cost is small:
flags arrive with `Append` for almost every message, so `StoreFlags` is a
second-run correction rather than the common path.

This database is **not** `state.db` and must never be merged with it. `state.db`
records the relationship between two endpoints for one sync; this records what
this store contains, and has to be readable by someone who knows nothing about
any sync that produced it.

## 3. Architecture: implement `imapx.Conn`

The local store is not a special case in the syncer. It is another `Conn`.

This is verified rather than hoped for. `pool.DialFunc` is
`func(context.Context) (imapx.Conn, error)` — the pool is defined against the
interface, not against `imapx.Dial`. `Syncer` holds two `*pool.Pool` and
nothing else, and no file in `internal/syncer` type-asserts to a concrete
connection. So a type satisfying the interface inherits, unchanged:

the connection pool and its governor, the state database, the four-tier
identity and adoption logic, message selection filters, `--delete2` and its
safety ceiling, the retry policy and the second pass, the byte budget, and the
run report.

Two further things fall out of the same decision:

**Restore is free.** Local → IMAP is the same sync with the endpoints swapped.
There is no second code path to write, and more importantly no second code path
to *forget to test*. This is the direct answer to the FAQ's warning.

**Local → local works** without being designed for, which makes the test suite
cheap: no server, no sockets, just two temporary directories.

A local "connection" is a handle on a directory. Dialling opens it; there is no
authentication and no socket. Several handles on one store are safe, which is
what lets the pool stay as it is. `Caps()` reports a modest, honest set — no
CONDSTORE, no QRESYNC, no UIDPLUS — and the existing capability fallbacks then
do the right thing without knowing why.

## 4. The seventeen methods

| Method | Local implementation |
| --- | --- |
| `Caps` | fixed, minimal set |
| `Namespaces` | unsupported; empty prefix |
| `ListFolders` | walk the tree for directories holding the folder database |
| `Select` | reconcile the directory against the database (§4.1) |
| `CreateFolder` | `mkdir` the folder and `.tmp/`, create the database |
| `SubscribeFolder` | a column in the folder's single-row table |
| `AllUIDs` | `readdir`, parse the leading digits — never the database |
| `FetchMeta` | read headers only, stop at the blank line |
| `FetchBody` | copy the file |
| `Append` | `.tmp/` → `fsync` → `rename` to `<uid>.eml`, then one INSERT |
| `SearchHeader` | scan headers; used only when adopting |
| `Search` | evaluate `searchkey.Key` against headers and `stat` |
| `FetchFlags` | one SELECT |
| `StoreFlags` | one UPDATE; the message file is not touched |
| `DeleteMessages` | `unlink`, then one DELETE |

`changedSince` on `FetchFlags` is ignored, which is correct rather than lazy: a
store reporting no CONDSTORE is never asked for a modseq it did not give out.

### 4.1 The filesystem is the truth; the database annotates it

The database will be wrong. Files get deleted in Finder, restored from a
backup, copied in from somewhere else, or moved while nothing is watching. A
design that treats `.imapsync-folder.db` as authoritative would report messages
that are gone and overlook messages that are there — and this project has
already met that exact bug from the other side. Finding 1 of the migration was
iCloud's SEARCH index listing 100,184 UIDs for a folder holding 487 messages;
the fix was to stop believing the index and cross-check it against reality.
Shipping the same bug in our own store, having just paid to learn it, would be
unforgivable.

So the rule is: **existence is a question for the directory, never for the
database.** `AllUIDs` reads `readdir`. The database supplies only what the
filesystem cannot express — flags, keywords, and the authoritative
INTERNALDATE.

`Select` reconciles, on names alone, which keeps it to one `readdir` with no
`stat` per file:

- **File with no row** — adopt it. Flags empty, INTERNALDATE from mtime. The
  next sync from a live source fills the flags in.
- **Row with no file** — the message was deleted outside the tool. Drop the
  row and count it in the report. This is a legitimate way to delete mail from
  the store, not an error.
- **A `.eml` whose name is not a UID** — someone dropped mail in by hand.
  Allocate the next UID, rename it into the store's form, adopt as above.

That last one is a feature rather than a tolerance: **mail can be added to the
store by copying files into a folder in Finder**, which falls out of the design
for free and is a large part of what makes the format worth having.

### 4.2 UIDs, and the one thing a lost database costs

A backup readable only alongside `state.db` is not a backup. The UID therefore
lives in the most durable place available — the message's own filename.
`0000009083.eml` is UID 9083 whatever else is lost.

The folder database also holds `uidvalidity`, `uidnext`, the folder's true IMAP
name and its subscription state, in a single-row table beside the per-message
one. `uidnext` is allocated under a per-folder lock held only for the
increment; writing the message is not serialised, which is the whole point.

If the database is lost, the messages and their UIDs are still on disk and
INTERNALDATE comes back from mtime. Flags and the folder's exact name spelling
are gone, and the next sync from a live source restores both.

`uidnext` is the exception, and it is the one place where guessing is not
allowed. Rebuilding it as the highest UID on disk plus one is wrong whenever
the highest-numbered messages were the ones deleted: the UIDs would be handed
out a second time, and IMAP promises within a UIDVALIDITY that they never are.
A client holding the old numbers would silently fetch the wrong mail. So a
folder that finds its database missing **generates a new UIDVALIDITY** rather
than risking reuse. That is the IMAP-legal way to say "my numbering is no
longer the one you remember", every client and this tool's own state database
already know how to respond to it, and the cost is one re-examination of a
folder in a situation that should never arise.

### 4.3 Flags

`\Seen`→`S`, `\Answered`→`R`, `\Flagged`→`F`, `\Deleted`→`T`, `\Draft`→`D`.
Keywords are stored verbatim; there is no need for dovecot's `a`–`z`
indirection once flags are no longer squeezed into a filename, and storing the
keyword as the server spelled it is what makes restore exact.

`\Recent` is not stored, because it is not a property of a message. It means
"arrived since another client last looked", it cannot be set by APPEND, and
this tool already strips it on the way out (`syncer.go`) and refuses to search
on it (`searchkey`). Recording it would be recording one server's passing mood.

The database maps UID to flag set. A message present on disk but absent from
the database — the window between `rename` and the INSERT, or a file someone
copied in — reads as having no flags, which §4.1 adopts and a later sync
corrects rather than a corruption. Messages are never rewritten to carry a
flag: a backup that edits the mail it stores is not a backup.

### 4.4 What has to survive for a restore to be exact

The point of the store is that an arbitrary IMAP server can be handed the
contents back. That fixes what must be kept, and the list is longer than the
messages.

**The message bytes**, unmodified. Nothing is rewritten, escaped or
re-encoded, which is the other half of why `.eml` is honest: the file is the
message.

**INTERNALDATE**, which is the one piece of per-message state that is *not* in
the message. `AppendMessage.InternalDate` already flows through the interface
into `APPEND`'s optional date-time, so a store that drops it would restore
776,802 messages all dated the day of the restore, with every client's sort
order destroyed and no way to notice until it was far too late. It is recorded
in the folder database, and *also* set as the file's mtime — the mtime is the
redundant copy and the one Finder shows, the database is the authority. Two
copies is not gratuitous here: unlike flags, INTERNALDATE cannot be re-fetched
from a source that no longer exists, which is the situation a backup is for,
and mtime is what lets §4.1 adopt a file that appeared from nowhere.

**Flags and keywords**, **folder names** as the server spelled them, and
**subscription state**.

What deliberately does not survive, because it cannot:

- **UIDs and UIDVALIDITY.** The destination assigns its own. The store's UIDs
  are local addressing, unrelated to the source's, and the state database is
  what maps between endpoints — as it already does for IMAP-to-IMAP.
- **MODSEQ.** The store reports no CONDSTORE, so it is never asked.
- **The hierarchy delimiter.** Stored as structure rather than as a character,
  so restoring to a server that separates with `.` works from a store filled by
  one that separates with `/`.

### 4.5 Folder names are the sharp edge

Three problems, none of them interesting and all of them capable of losing
mail.

- **Delimiters vary.** iCloud uses `/`, others `.`. The store mirrors the IMAP
  hierarchy as nested directories, so `Archive/2024` is two levels. A
  subdirectory is a subfolder and a `.eml` is a message, so the two can never
  be confused for one another.
- **Filesystem-unsafe characters.** Percent-encode anything outside a
  conservative set, and keep the true name in the folder database so restore
  spells it exactly as the server did.
- **macOS is case-insensitive.** Two IMAP folders differing only in case are
  legal and would collide on this user's own machine. Detect the collision and
  fail the folder loudly; do not silently merge two folders into one, which
  would be indistinguishable from mail loss.

## 5. Cost, honestly

`FetchMeta` over a folder with no state database entry reads every header in
that folder — 413,954 files for the INBOX in the example. On an SSD that is
minutes, not hours, and it happens only when adopting an existing store rather
than on every run. It is the same shape as `indexDestination` today. If it
proves painful the folder database is the obvious place to cache Message-ID and
size, but that should not be built before the pain is measured.

The reconciliation of §4.1 is a `readdir` per folder per run and no `stat`,
which is the cheap half of the same walk.

## 6. Testing

The existing suite tests against an in-memory IMAP server; the local store
needs no server at all, so `t.TempDir()` is the whole fixture.

These deserve tests that would fail without them:

- **Round trip to a *different* server.** IMAP → local → a second, empty IMAP
  account, compared against the first: same messages byte for byte, same
  flags and keywords, same INTERNALDATE, same folder tree — including a source
  and destination whose hierarchy delimiters differ, since that is the case a
  structural layout exists to handle. This is the restore the FAQ warns about,
  asserted rather than assumed, and it is the test the whole design is for.
- **Every way the directory and the database can disagree**, since §4.1 is the
  part most likely to be got wrong. Delete a `.eml` behind the store's back and
  it must vanish from `AllUIDs` rather than be fetched. Drop a foreign `.eml`
  in and it must be adopted, renamed and appear. Delete the database entirely
  and the folder must come back with a new UIDVALIDITY, its messages intact and
  their dates recovered from mtime — and specifically **must not reuse a UID**
  after the highest-numbered message was deleted, which is the case that would
  hand two different messages the same number.
- **Crash during append.** A message left in `.tmp/` is not visible as a
  `.eml` and is not counted, so the existing in-flight recovery settles it.
- **The case collision.** Two folders differing only in case must fail rather
  than merge.
- **Double-clickable.** A written message is byte-identical to what the server
  gave us, so the point of the `.eml` name holds.

Plus the ordinary mutation discipline: break each of the flag mappings, the
UID-from-filename parse, the percent-encoding, and each arm of the
reconciliation, and confirm a named test fails.

## 7. Milestones

- **L0** — the store: create, list, select, append, fetch, flags, delete, UID
  allocation, and the reconciliation of §4.1. Local → local sync in tests.
- **L1** — wire it into config and the `sync` command as either endpoint.
  IMAP → local backup works end to end.
- **L2** — restore, which is L1 with the endpoints swapped, plus the round-trip
  test that proves it.
- **Later, if asked** — one-way mbox export for other tools; a header index if
  L1 measures the scan as painful.

## 8. Decisions and what they cost

1. **`<uid>.eml`, and therefore not a maildir.** Double-clickable in Finder and
   Explorer. No maildir-aware tool — mutt, mbsync, dovecot — can read the
   store, and that is a non-goal rather than a price: the store exists to be
   handed back to an arbitrary IMAP server by this tool, and to be opened by a
   person in between. Feeding other mail software was never the job.
2. **Nested directories rather than Maildir++.** Legible, and no dot-escaping.
   The compatibility this gives up was already gone with decision 1.
3. **Flags in a per-folder SQLite database, not the filename.** Message files
   are then written once and never touched, so an incremental backup of the
   store copies only the mail that actually arrived. SQLite rather than JSON
   because a document has to be rewritten whole to change one entry.
4. **The filesystem outranks the database.** Existence is answered by
   `readdir`; the database only annotates. This is the decision most likely to
   be quietly abandoned under performance pressure, and the one that must not
   be.
5. **UID in the filename** rather than only in the database, so the store
   survives losing both `state.db` and its own bookkeeping.
6. **A missing folder database means a new UIDVALIDITY**, because a rebuilt
   `uidnext` can reuse numbers that deleted messages already had.
7. **Restore in the first milestones**, on the FAQ's warning.

Still genuinely open, and cheap to settle at L0:

- **Whether one directory per folder scales to a Finder window.** 413,954 files
  in `INBOX/` is fine for APFS and unpleasant for Finder. Sharding by UID
  prefix would fix the machine's problem by ruining the human's, so it is not
  proposed — but the number should be looked at once rather than assumed.
- **Whether the subject belongs in the filename.** `0000009083.eml` is opaque
  when browsing. `0000009083 Re Invoice for March.eml` is not, and costs
  sanitising, length limits, and a rename if the design ever lets a subject
  change. Deferred until the store is being browsed in anger.
- **What a backup tool copying the store mid-write sees.** SQLite's WAL means
  a naive copy can catch a torn database. Since the messages survive it and
  §4.1 rebuilds from them, the damage is bounded — but the store should
  probably checkpoint and close when idle, and that should be measured rather
  than assumed.
