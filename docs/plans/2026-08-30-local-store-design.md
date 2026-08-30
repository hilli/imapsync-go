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

That compatibility was already spent. §8's first decision mirrors the IMAP
hierarchy as ordinary nested directories rather than Maildir++, which mbsync
cannot read either. Half-honouring a convention nothing can use is worse than
declaring the format, so:

```
Archive/                       a folder
Archive/.imapsync-folder.json  its metadata, hidden from Finder
Archive/.tmp/                  staging, same filesystem, for atomic rename
Archive/0000009083.eml         a message
Archive/2024/                  a subfolder
```

A directory is a folder; a `.eml` is a message. Opening `Archive` in Finder
shows the mail and nothing else. UIDs are zero-padded to ten digits so that
lexical order — which is what every file browser and `ls` shows — is UID order.

Kept from maildir: one file per message, write-`fsync`-rename delivery, delete
by `unlink`, and no lock anywhere on the message path. Dropped: `cur/new/tmp`,
the colon, and flags in the filename.

### 2.2 Message files are immutable

Flags do not go in the filename. They go in the folder's metadata file.

Maildir changes flags by renaming, which means a message read once is a
different filename for ever after. For a store whose point is to be backed up
incrementally that is the wrong trade: `\Seen` sweeping a folder would rename
thousands of files, and a backup tool without rename detection would copy every
one of them again. Content-addressed tools survive it; `rsync` does not.

With flags in the index, **a message file is written exactly once and never
touched again.** The only file that changes as flags move is one small JSON
document per folder. That is the property that makes an incremental backup of
this store proportional to the mail that actually arrived.

It costs a lock the filename approach did not need, and that cost is small:
flags arrive with `Append` for almost every message, so `StoreFlags` is a
second-run correction rather than the common path.

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
| `ListFolders` | walk the tree for directories holding the metadata file |
| `Select` | read the folder's metadata file |
| `CreateFolder` | `mkdir` the folder and `.tmp/`, write metadata |
| `SubscribeFolder` | a flag in the folder's metadata |
| `AllUIDs` | read the directory, parse the leading digits |
| `FetchMeta` | read headers only, stop at the blank line |
| `FetchBody` | copy the file |
| `Append` | `.tmp/` → `fsync` → `rename` to `<uid>.eml` |
| `SearchHeader` | scan headers; used only when adopting |
| `Search` | evaluate `searchkey.Key` against headers and `stat` |
| `FetchFlags` | read the folder's flag index |
| `StoreFlags` | update the flag index; the message is not touched |
| `DeleteMessages` | `unlink`, and drop the index entry |

`changedSince` on `FetchFlags` is ignored, which is correct rather than lazy: a
store reporting no CONDSTORE is never asked for a modseq it did not give out.

### 4.1 UIDs must survive the state database

A backup readable only alongside `state.db` is not a backup. The mapping
therefore lives in the store, and in the most durable place available: the
message's own filename. `0000009083.eml` is UID 9083 whatever else is lost.

Each folder carries `.imapsync-folder.json`, holding its `uidvalidity`, its
`uidnext`, the folder's true IMAP name, its subscription state, and the flag
index. If that file is lost, everything except the flags and the exact name
spelling is rebuilt by reading the directory — `uidnext` is the highest UID
plus one — so the store is self-describing and self-healing, and the flags
are restored by the next sync from the source.

`uidnext` is allocated under a per-folder lock held only for the increment.
Writing the message is not serialised, which is the whole point.

### 4.2 Flags

`\Seen`→`S`, `\Answered`→`R`, `\Flagged`→`F`, `\Deleted`→`T`, `\Draft`→`D`,
`\Recent`→`N`. Keywords are stored verbatim; there is no need for dovecot's
`a`–`z` indirection once flags are no longer squeezed into a filename, and
storing the keyword as the server spelled it is what makes restore exact.

The index maps UID to flag set. A message present on disk but absent from the
index — the window between `rename` and the index write — reads as having no
flags, which is a state a later sync corrects rather than a corruption.
Messages are never rewritten to carry a flag: a backup that edits the mail it
stores is not a backup.

### 4.3 Folder names are the sharp edge

Three problems, none of them interesting and all of them capable of losing
mail.

- **Delimiters vary.** iCloud uses `/`, others `.`. The store mirrors the IMAP
  hierarchy as nested directories, so `Archive/2024` is two levels. A
  subdirectory is a subfolder and a `.eml` is a message, so the two can never
  be confused for one another.
- **Filesystem-unsafe characters.** Percent-encode anything outside a
  conservative set, and keep the true name in the metadata file so restore
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
proves painful the answer is a header index in the store, but it should not be
built before the pain is measured.

## 6. Testing

The existing suite tests against an in-memory IMAP server; the local store
needs no server at all, so `t.TempDir()` is the whole fixture.

Three things deserve tests that would fail without them:

- **Round trip.** IMAP → local → IMAP, with the second IMAP compared to the
  first: same messages, same flags, same internal dates. This is the restore
  the FAQ warns about, asserted rather than assumed.
- **Crash during append.** A message left in `.tmp/` is not visible as a
  `.eml` and is not counted, so the existing in-flight recovery settles it.
- **A message on disk with no index entry** reads as flagless and is corrected
  by the next sync, rather than being reported as missing.
- **The case collision.** Two folders differing only in case must fail rather
  than merge.
- **Double-clickable.** A written message is byte-identical to what the server
  gave us, so the point of the `.eml` name holds.

Plus the ordinary mutation discipline: break each of the flag mappings, the
UID-from-filename parse, and the percent-encoding, and confirm a named test
fails.

## 7. Milestones

- **L0** — the store: create, list, select, append, fetch, flags, delete, with
  metadata and UID allocation. Local → local sync in tests.
- **L1** — wire it into config and the `sync` command as either endpoint.
  IMAP → local backup works end to end.
- **L2** — restore, which is L1 with the endpoints swapped, plus the round-trip
  test that proves it.
- **Later, if asked** — one-way mbox export for other tools; a header index if
  L1 measures the scan as painful.

## 8. Decisions and what they cost

1. **`<uid>.eml`, and therefore not a maildir.** Double-clickable in Finder and
   Explorer, at the price that no maildir-aware tool — mutt, mbsync, dovecot —
   can read the store. Confirmed as the priority: the store is meant to be
   opened by a person.
2. **Nested directories rather than Maildir++.** Legible, and no dot-escaping.
   The compatibility this gives up was already gone with decision 1.
3. **Flags in a per-folder index, not the filename.** Message files are then
   written once and never touched, so an incremental backup of the store copies
   only the mail that actually arrived. Costs a lock on a path that is mostly
   exercised by `Append`, which is carrying the flags anyway.
4. **UID in the filename** rather than only in the index, so the store survives
   losing both `state.db` and its metadata.
5. **Restore in the first milestone**, on the FAQ's warning.

Still genuinely open, and cheap to settle at L0:

- **Whether one directory per folder scales to a Finder window.** 413,954 files
  in `INBOX/` is fine for APFS and unpleasant for Finder. Sharding by UID
  prefix would fix the machine's problem by ruining the human's, so it is not
  proposed — but the number should be looked at once rather than assumed.
- **Whether the subject belongs in the filename.** `0000009083.eml` is opaque
  when browsing. `0000009083 Re Invoice for March.eml` is not, and costs
  sanitising, length limits, and a rename if the design ever lets a subject
  change. Deferred until the store is being browsed in anger.
