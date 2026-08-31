# imapsync-go

A Go implementation of the ideas behind Perl [imapsync]: one-way migration and
repeated synchronisation of IMAP accounts.

The reason to rewrite it is **concurrency**. IMAP is strictly request/response
per connection, so throughput comes from running many connections and slicing
work across them — folder-parallel *and* intra-folder parallel, so a single
200k-message INBOX still saturates the available connections.

> **Status:** M0–M5 complete. `probe` reports what a server supports, `sync`
> performs a correct, resumable, concurrent one-way copy across many
> connections, `--delete2` mirrors deletions, `--max-size`/`--max-age` and
> their opposites select which messages move, and `compat` runs imapsync
> command lines. See [the design document](docs/plans/2026-08-27-imapsync-go-design.md).

## Install

Homebrew (macOS, Linux):

```sh
brew install hilli/tap/imapsync-go
```

Otherwise grab a binary or `.deb`/`.rpm`/`.apk` from the [releases page], or
build from source.

## Build and run

With [Task] (`brew install go-task`):

```sh
task build       # -> bin/imapsync-go
task test        # go test ./...
task lint        # golangci-lint run ./...
task complete    # generate shell completions into completions/
task --list      # everything else
```

Or plain Go:

```sh
go build -o imapsync-go ./cmd/imapsync-go
```

To run without building:

```sh
go run ./cmd/imapsync-go probe --help
```

## Shell completion

Cobra generates completions for bash, zsh, fish and PowerShell. The Homebrew
cask and the Linux packages install them for you; otherwise:

```sh
imapsync-go completion zsh > "$(brew --prefix)/share/zsh-completions/_imapsync-go"
imapsync-go completion bash > "$(brew --prefix)/share/bash-completion/completions/imapsync-go"
```

## probe

`probe` connects to an endpoint and reports what it can support: negotiated
capabilities, namespace and hierarchy delimiter, the folder list with
SPECIAL-USE attributes, and optionally the practical simultaneous connection
limit.

Server connection limits are not discoverable from capabilities, and learning
them mid-migration is the expensive way to find out.

```sh
export ICLOUD_APP_PW='xxxx-xxxx-xxxx-xxxx'

go run ./cmd/imapsync-go probe \
    --url 'imaps://you@icloud.com@imap.mail.me.com' \
    --password-env ICLOUD_APP_PW \
    --status \
    --max-connections 8
```

Note the username needs no percent-encoding: everything before the **last** `@`
is treated as the username, so an email address works as-is.

`--max-connections` opens connections until the server refuses, and it is off by
default because it is intrusive. Read the result carefully: if the search
reaches the number you gave without being refused, no limit was found, and the
report says so rather than calling your own cap a ceiling. Measured here, mox
stopped dead at 30 while iCloud never refused at all.

Add `--json` for machine-readable output.

### Diagnosing server quirks

IMAP servers disagree about which extensions they honour, and some advertise
capabilities they then refuse to use. `--trace` prints the raw conversation to
stderr so a rejection is visible rather than inferred:

```sh
go run ./cmd/imapsync-go probe --url '...' --password-env ICLOUD_APP_PW --trace
```

Credentials are redacted before anything is written, so a trace is safe to save
and share.

### URL format

| Scheme | Transport | Default port |
|---|---|---|
| `imaps://user@host` | implicit TLS | 993 |
| `imap://user@host` | STARTTLS | 143 |
| `imap+insecure://user@host` | plaintext, test use only | 143 |
| `file:///path/to/directory` | a directory on disk | — |

Inline passwords (`imaps://user:pass@host`) are rejected. Credentials come from
`--password-env`, `--password-file` or `--password-keychain` (macOS). A
`file://` endpoint is a directory and takes no password; supplying one is an
error rather than something quietly ignored.

## sync

`sync` copies every message the destination does not already hold.

```sh
go run ./cmd/imapsync-go sync \
    --source-url 'imaps://you%40example.com@imap.mail.me.com' \
    --source-password-env ICLOUD_APP_PW \
    --dest-url imaps://you@mox.example.net \
    --dest-password-keychain mox-imap
```

```
Created 1 destination folder: Work

SOURCE  DESTINATION  MESSAGES  COPIED  ADOPTED  ALREADY  FAILED
INBOX   INBOX        2         2       0        0        0
Work    Work         1         1       0        0        0

2 folders, 3 copied, 0 adopted, 0 failed, in 5ms (600.0 messages/second)
```

Use `--dry-run` first: it reports the folders it would create and the messages
it would copy, and writes nothing to either the destination or the state
database.

### Going faster

IMAP does not multiplex: one connection carries one command at a time, and a
single connection to a distant server spends nearly all of it waiting. Speed
therefore comes from opening more connections, and `sync` uses them two ways at
once — several folders in flight, and several connections sharing one folder.
Both are needed. Where a single mailbox holds half an account, dividing the work
only by folder leaves one connection with half the job; dividing it only within
a folder leaves the remaining mailboxes to trickle through one at a time.

| Flag | Default | Effect |
| --- | --- | --- |
| `--source-connections` | 4 | Connections to the source, and the number of folders in flight |
| `--dest-connections` | 8 | Connections to the destination |
| `--memory-limit` | 256MiB | Ceiling on message bodies held in memory at once |
| `--progress-interval` | 30s | How often to report what the run has done; `0` to keep quiet |

The destination gets more connections than the source because appending is the
slower half: the server has to accept, store and index a whole message where the
source only has to read one back.

Asking for more connections than a server will hold is not a disaster. When a
connection is refused while the others are working normally, the pool takes that
as the server's answer, drops to the number currently open, and closes what it
cannot keep. The refused operation retries; nothing is lost. The run ends by
telling you the width it settled on and which flag to set to start there next
time, which is the only measurement of that server anyone has:

```
The destination server would not hold 16 connections; the run settled on 12.
Pass --dest-connections=12 next time to start there and skip the refusals.
```

The pool never grows back within a run. Servers do not announce that a limit has
lifted, so growing means probing for a refusal, and a refusal is what we were
avoiding.

Raise the counts gradually all the same. Not every server says plainly that you
have opened too many, and a limit met as a stall rather than a refusal is
invisible to this. If a run starts failing or stalling after a change, lower the
number rather than retrying: state is written as the copy proceeds, so a re-run
resumes.

Bodies are held in memory between the fetch and the append, never spooled to
disk. `--memory-limit` bounds that. A message larger than the whole limit is
still copied, alone, so no message is too big to sync. Accepts `512MiB`, `2GB`,
`1G` or a plain byte count.

### When the network misbehaves

Connections to a distant server break, and a sync of a large account runs long
enough to be certain of it. A dropped connection is reconnected and the work
resumes at the message that failed, rather than at the start of the batch or by
abandoning the folder. Servers that ask to be left alone — `LIMIT`, `INUSE`, or
iCloud's "too many simultaneous connections" — are backed off further than a
plain disconnect, by a randomised amount, so that connections which all failed
together do not all come back together.

A message the server will never accept, because it is too large or the mailbox
is full, is reported and skipped rather than retried. It is not written off: the
next run tries it again, since only messages that were copied are skipped when a
run repeats.

Folders that failed are attempted once more at the end of the run, on fresh
connections. Whatever stopped them has often stopped being true by then.

If the destination stops accepting mail altogether, the run ends rather than
spending hours discovering that one message at a time. The threshold is fifty
failures in a row with nothing copied in between; scattered failures across a
long run do not count against it.

While a long run is working, it reports what it has copied every 30 seconds, so
that a slow folder can be told apart from a hang. `--progress-interval 0` turns
that off.

### It is safe to interrupt

Progress is recorded as the copy proceeds, in a SQLite database under your user
configuration directory (override with `--state`). Re-running skips what is
already there:

- **Already** counts messages the state database has recorded. A second run of
  an unchanged account is entirely this column.
- **Adopted** counts messages found already present at the destination and
  recorded rather than copied again. This is what happens when the state
  database is lost, when the destination was not empty to begin with, or when a
  message's append was in flight at the moment the process stopped.

Deleting the state database costs a pass over the destination's headers, not a
duplicated account.

Interrupting a run prints what it managed to copy before it stopped, which is
what the next run will not have to do again. Press Ctrl-C once and the run winds
down: in-flight appends are recorded before it exits, so an interrupt costs a
few seconds rather than a folder.

### Skipping folders nothing has touched

A second run over an account that has not changed should not have to look at
every message to find that out. Where the server supports CONDSTORE — iCloud
does — each folder is asked for one number at `SELECT` time, and a folder whose
number has not moved is skipped in that single command: no message listing, no
destination connection.

The number is only recorded by a run that finished the folder with nothing
failed, so a message that could not be copied is always tried again rather than
being silently written off.

What this cannot see is the destination. A message deleted at the far end, or a
mailbox renumbered there, leaves the source's number untouched. `--full` ignores
the shortcut and compares every folder properly:

```sh
imapsync-go sync ... --full
```

### Flags

Read, answered, flagged and your own keywords are copied with each message, and
kept in step afterwards: a message you read on the source is marked read on the
destination on the next run, and one you mark unread again comes back unread.
This is on by default, as it is in imapsync. `--noresyncflags` turns it off.

Finding out what changed is the expensive part, and CONDSTORE makes it cheap —
the server is asked only for the flags that moved since the last run, rather than
for all 414,000 of them. On a folder the shortcut above skipped, nothing is
asked at all.

Only flags travel. This is a one-way mirror, so a message you read on the
*destination* will be marked unread again to match the source.

### Subscriptions

Folders created by a run are subscribed, as imapsync does by default. A mail
client browsing by subscription would otherwise not show them, which looks
exactly like the mail never having been copied. `--subscribe=false` turns it
off.

Only newly created folders are touched: a folder you deliberately unsubscribed
from stays that way. A server that refuses SUBSCRIBE gets a warning rather than
a failed run.

### Numbers with no message behind them

Some servers list UIDs they have nothing to give you. iCloud is one: on an INBOX
holding 414,053 messages it lists 503,786 UIDs, and roughly ninety thousand of
them have no message. This is not a mid-run deletion and not an error — asking
for one of those messages succeeds and simply says nothing.

Those are reported separately as VANISHED, and they are neither loss nor
failure: nothing was skipped that the server actually had. They are also written
down, so the next run does not ask about them again. Without that, iCloud's
INBOX would re-request ninety thousand headers on every single run, for ever.

The same column counts messages genuinely deleted from the source between the
moment the run listed them and the moment it went to read them.

### Deleting what the source no longer has

By default nothing is ever deleted: `sync` only adds. `--delete2` asks for the
destination to follow the source when mail leaves it.

```console
imapsync-go sync --source-url ... --dest-url ... --delete2
```

This is imapsync's `--delete2`: the destination ends up looking like the source,
including losing mail that got there some other way. Messages this tool copied
are matched by UID, which is exact. Everything else on the destination is
matched by identity — the same digest used to adopt messages already present —
and deleted if the source has nothing like it.

One thing is deliberately left alone: a message carrying too little header to
identify. Adoption already refuses to match on those, because a wrong match
drops mail, and "the source has nothing like this" is not a conclusion you can
draw from a digest that could not have recognised it either way. They stay, and
the count is logged.

The cost falls where it should. On a mirror this tool built, every message is
accounted for in the state database, so the check is one UID listing and no
header reads at all. Only mail that arrived some other way is paid for, and only
once.

Deleting needs UIDPLUS, and the run stops rather than working around a server
that lacks it. Plain `EXPUNGE` purges *every* message in the mailbox flagged
`\Deleted`, including ones you flagged by hand in your own mail client, so
`UID EXPUNGE` is required rather than merely preferred.

There is a safety valve. A source that answers a UID listing with nothing, or
with a fraction of the truth, looks exactly like a source whose mail has been
deleted — and the response to those two is very different. So a run refuses to
delete more than a tenth of a destination folder at once:

```
REFUSED to delete 47 messages. That is a larger share of the destination folder
than --delete2-ceiling allows to go in one run, and the usual cause is a source
that answered a listing with less than the truth.
  Archive: 47 of 312

Check the source, then pass --force to go ahead, or raise --delete2-ceiling.
```

A refusal is not a failure and nothing is lost, but the run exits non-zero, so a
scheduled sync cannot quietly stop mirroring a folder while still reporting
success. `--force` carries out the deletions anyway, and `--delete2-ceiling`
moves the line.

The first `--delete2` run against a destination that has mail of its own will
usually refuse for exactly this reason, which is the right way to find out.

A handful of messages is always allowed through whatever share of the folder
they are, because one message out of six is 16.7% and refusing that would mean
`--force` living permanently in your cron line — at which point the ceiling is
protecting nothing.

`--dry-run` reports what would be deleted without touching anything, and it
computes it with the same code the real run uses, including the ceiling.

Folders are left alone entirely if anything in them failed to copy.

Turning `--delete2` on for the first time works on an account you have been
syncing for months. Folders record separately how far copying and deleting have
each got, so adding the flag examines every folder once, even ones the server
says have not changed — otherwise the deletions made while the flag was off
would never be carried out. After that the fast path resumes as normal, and a
refusal is offered again on the next run rather than being final.

### Choosing folders

| Flag | Effect |
| --- | --- |
| `--folder NAME` | copy only these source folders, by exact name |
| `--include REGEX` / `--exclude REGEX` | filter by pattern |
| `--map SOURCE=DEST` | map one folder explicitly |
| `--subfolder2 NAME` | nest the whole tree under one destination folder |
| `--automap` | map `Sent`, `Trash` and friends onto the destination's own names (default) |
| `--include-virtual` | also copy virtual mailboxes such as Gmail's All Mail |

Virtual mailboxes are **skipped by default**, which differs from imapsync.
Gmail's All Mail is a view over every other folder, so copying it duplicates the
account.

### Choosing messages

Within the folders it copies, `sync` can leave out messages by size, by age,
or by asking the server an IMAP `SEARCH` question.

| Flag | Effect |
| --- | --- |
| `--max-size SIZE` | skip messages of `SIZE` or larger |
| `--min-size SIZE` | skip messages of `SIZE` or smaller |
| `--max-age AGE` | skip messages older than `AGE` |
| `--min-age AGE` | skip messages newer than `AGE` |
| `--age-basis BASIS` | which date the age bounds read: `sent` (default) or `internal` |

Sizes take the same spellings as `--memory-limit` (`25MiB`, `2GB`). Ages take
days or any Go duration (`30d`, `0.5d`, `36h`).

```bash
# everything from the last month, nothing over 25 MiB
imapsync-go sync --source-url ... --dest-url ... --max-age 30d --max-size 25MiB
```

The size bounds are **strict** and the age bounds are **inclusive**: `--max-size
25MiB` skips a message of exactly 25 MiB, while `--max-age 30d` keeps one of
exactly thirty days. That asymmetry is imapsync's, and it is reproduced here on
purpose — a drop-in that quietly moved a boundary would copy a different set of
messages than the tool it replaces.

Giving both age bounds does something surprising, and it is imapsync's own
behaviour, described in its help as "magic!":

- `--max-age 30d --min-age 7d` keeps a **window** — between one and four weeks old.
- `--max-age 7d --min-age 30d` keeps the **union** — newer than a week *or*
  older than a month.

When the two zones overlap you get their intersection; when they do not, you get
both ends rather than nothing.

Age is measured from the message's `Date:` header — when the sender says it was
written — which is what imapsync does. Pass `--age-basis internal` to measure
from INTERNALDATE instead, the arrival time in the mailbox; that is imapsync's
`--noabletosearch`, and `compat` translates it for you.

| Basis | Reads | Notes |
| --- | --- | --- |
| `sent` (default) | the `Date:` header | belongs to the message, so it survives a migration |
| `internal` | INTERNALDATE | belongs to the mailbox, and cannot be forged by a sender |

The two agree for almost all mail. They diverge for anything that has itself
been moved between servers, which arrives carrying the date of the migration
rather than of its delivery — so on a mailbox that was migrated last week,
`--age-basis internal --max-age 30d` selects everything, while the default
selects what was actually written in the last month.

A message with no `Date:` header, or one no parser accepts, falls back to its
INTERNALDATE rather than being dropped. Drafts and script-generated mail are
routinely undated, and excluding them silently would be the worse answer.

The report counts what was left out in a `FILTERED` column, separately from
failures, so a run that copies less than expected says so. A `--dry-run` with a
filter set fetches message metadata in order to give a real answer, which costs
more than an unfiltered dry run does.

Nothing is recorded in the state database for a filtered message, so raising a
bound later picks it up. The cost is that a permanently-excluded message is
re-examined on every run — the same cost imapsync pays, having no state database
at all. For the same reason a run that filtered anything does not record a
"folder fully mirrored" watermark, and so cannot use the fast path described
under [Skipping folders nothing has touched](#skipping-folders-nothing-has-touched).
Without that, `--min-age 7d` would skip a folder for ever rather than pick up
messages as they aged into range.

If the destination advertises `APPENDLIMIT`, it is enforced as a `--max-size`
the server itself asked for, and combined with yours by taking whichever is
smaller.

#### Choosing messages with IMAP SEARCH

For anything the size and age bounds cannot express, the server can be asked
directly:

| Flag | Effect |
| --- | --- |
| `--source-search KEY` | copy only source messages the search matches |
| `--dest-search KEY` | let `--delete2` remove only destination messages the search matches |

```bash
# only unread mail under 100 kB
imapsync-go sync --source-url ... --dest-url ... \
  --source-search "UNSEEN SMALLER 100000"

# only mail from one sender, sent this decade
imapsync-go sync --source-url ... --dest-url ... \
  --source-search 'FROM "billing@example.com" SENTSINCE 1-Jan-2020'
```

The search runs on the server before anything is fetched, so a key that
excludes most of a folder makes the run proportionately cheaper — unlike
`--max-size`, which has to see a message's metadata to know its size.

Several keys in a row mean **all** of them: `SEEN SMALLER 10000` is both
conditions, not either. `OR` takes exactly two keys and `NOT` takes one, as in
`OR (FROM "a@example.com") (FROM "b@example.com")`. Dates are written the way
IMAP writes them — `1-Feb-2020` — and strings that contain spaces are quoted.

Supported keys: `ALL` `ANSWERED` `UNANSWERED` `DELETED` `UNDELETED` `DRAFT`
`UNDRAFT` `FLAGGED` `UNFLAGGED` `SEEN` `UNSEEN` `KEYWORD` `UNKEYWORD`
`FROM` `TO` `CC` `BCC` `SUBJECT` `HEADER` `BODY` `TEXT` `LARGER` `SMALLER`
`SINCE` `BEFORE` `ON` `SENTSINCE` `SENTBEFORE` `SENTON` `UID` `NOT` `OR`.

Three things are refused rather than approximated, each because accepting them
would quietly search for something other than what was asked:

- **`RECENT`, `NEW` and `OLD`** rest on `\Recent`, which means "arrived since
  another client last looked". The server clears it for whichever client gets
  there first, so two runs against one mailbox disagree and the second is
  wrong. IMAP4rev2 removed the flag.
- **A bare sequence set** such as `1:5` names positions in the mailbox rather
  than messages, and positions shift whenever anything is expunged. Write
  `UID 1:5` to name messages.
- **`LARGER 0` and `SMALLER 0`**, which would be dropped on the way to the
  wire, leaving `SMALLER 0` — a search matching nothing — as a search matching
  the entire mailbox.

A run carrying a search does not record a folder as fully mirrored, for the
same reason a filtered run does not: `UNSEEN` stops being true when somebody
reads their mail, and a watermark would skip the folder for ever.

##### `--dest-search` is narrower here than imapsync's `--search2`

In imapsync, `--search2` hides destination messages from *everything*, so a
message it hides is not recognised as already copied and is copied a second
time; imapsync's own documentation warns about this.

Here the search is applied to the deletion candidates alone. It cannot reach
the check that recognises a message as already present, so it can only ever
delete **fewer** messages — never copy more, and never duplicate anything. It
also cannot nominate a message the source still holds: a search is a
narrowing, and only a narrowing.

Because it narrows nothing but deletion, `--dest-search` without `--delete2`
does nothing at all, and says so.

### Self-signed servers

TLS verification is controlled **per side**: `--source-insecure` and
`--dest-insecure`. A self-signed destination on your own network is a
reasonable thing to accept; accepting it must not also stop verifying a public
source reached over the internet, so there is no single flag that does both.

macOS rejects a self-signed certificate whose validity exceeds 398 days with
`certificate is not standards compliant`, and adding it to the keychain does not
help. Either reissue it with a shorter lifetime or pass `--dest-insecure`.

## Backing up to a directory

A `file://` endpoint is a directory of `.eml` files that behaves like an IMAP
server, so it can be either side of a sync. imapsync has no equivalent, so
neither does `compat`.

```sh
# Back an account up to disk
imapsync-go sync --source-url imaps://you@imap.example.net \
    --source-password-keychain example-imap \
    --dest-url file://~/Mail-backup

# Put it back on a server — any server, not the one it came from
imapsync-go sync --source-url file://~/Mail-backup \
    --dest-url imaps://you@imap.other.net \
    --dest-password-keychain other-imap
```

Everything the rest of this document describes still applies: the same
concurrency, the same state database, the same folder and message filters, the
same interruptible resume. Only the URL changed.

### What it looks like on disk

One directory per folder, one file per message, named by UID:

```
Mail-backup/
  INBOX/
    .imapsync-folder.db
    0000000001.eml
    0000000002.eml
    +0000010000/          folders over 10,000 messages shard by UID
      0000010001.eml
```

One file per message is what makes an incremental backup cheap: a run that
changes nothing writes nothing, so the next `restic`, `borg` or Time Machine
pass stores only what actually arrived. Flags, UIDs and UIDVALIDITY live in the
SQLite database beside the messages, and each file's creation and modification
time is set to the message's own date, so a 1999 message reads as 1999 in
Finder.

The database is a cache, not the truth. Delete a message and the next run
notices it is gone; delete the database and the folder is rebuilt from the
files, under a new UIDVALIDITY so no peer mistakes it for the one it replaced.

The store is quiescent between runs — the write-ahead log is checkpointed and
removed on close — so a backup taken while nothing is syncing cannot capture a
transaction in progress. Run the sync before the backup rather than during it.

Folder names are escaped only where the filesystem forces it, so `Arkiv/2019`
nests as directories and stays readable. `.eml` was chosen over maildir's
naming so the files open by double-clicking; the maildir mechanics that matter
— one file per message, written to a temporary name and renamed into place —
are kept.

### A source has to exist, a destination is created

A `file://` source that is not there is an error. The asymmetry is deliberate:
a mistyped restore path would otherwise be created empty, diffed against a
destination that already had everything, and reported as a clean run with
nothing to do.

`--dry-run` creates nothing at all, including the destination.

## Running imapsync command lines

Existing imapsync invocations — the ones in your cron table and your notes —
can be run through the `compat` shim, which translates them and runs the
resulting `sync`:

```sh
imapsync-go compat --host1 imap.mail.me.com --user1 you@example.com \
  --passfile1 ~/.icloud --ssl1 \
  --host2 mail.example.net --user2 you --passfile2 ~/.mox --ssl2 \
  --folder Archive --dry
```

Symlink the binary to make it a genuine drop-in, so scripts that call
`imapsync` by name keep working without being edited:

```sh
ln -s "$(command -v imapsync-go)" /usr/local/bin/imapsync
```

The shim prints the translation before running it:

```
translated to:
  imapsync-go sync --source-url imaps://you%40example.com@imap.mail.me.com:993 \
    --source-password-file /Users/you/.icloud \
    --dest-url imaps://you@mail.example.net:993 \
    --dest-password-file /Users/you/.mox --folder Archive --dry-run
```

That is the command that runs, not an approximation of it, so it can be copied
into a bug report or kept as the native version of a script you are migrating.

### It refuses rather than guesses

A flag that changes **which messages move, or what becomes of them** is refused
if this tool cannot honour it exactly. `--regextrans2`, `--truncmess`,
`--gmail1` and the rest stop the run and say so, all of them at once rather
than one per attempt.

The alternative — accepting a flag and quietly not applying it — fails in the
worst possible way. `--maxage 30` silently ignored copies twelve years of mail
instead of a month, and you find out from a full disk. (`--maxage` is now
translated; it is named here because it is the clearest example of the damage.)

```
error: 2 imapsync options cannot be honoured:
  --regextrans2: folder-name rewriting by regular expression is not
            implemented; --map renames folders one at a time
  --justfolders: copying folders without their messages is not implemented;
            --dry-run reports the plan instead
```

Flags that ask for something that already happens are **ignored and reported**,
never silently dropped:

```
accepted but did nothing:
  --addheader     this tool stamps only the messages that have no Message-ID, and
                  its digest covers a fixed list of header fields, so a stamp
                  cannot change how anything is matched
  --exchange1     it does nothing in imapsync either
  --useuid        the state database records what has been copied, so this is
                  not needed
```

### Passwords

`--password1` and `--password2` are moved into the environment rather than
passed as arguments, because arguments are readable by every process on the
machine. `--passfile1`/`--passfile2` are better still and translate directly.

### One assumption it has to make

Given neither `--ssl1` nor `--tls1`, imapsync probes port 993 and uses TLS if
something answers. Doing that here would make translation depend on the
network. The shim assumes TLS instead and says so:

```
assumed: neither --ssl1 nor --tls1 was given, so TLS on port 993 was assumed;
say --tls1 for STARTTLS, or --nossl1 for no encryption
```

It never assumes in the other direction: nothing you type gets quietly
downgraded to an unencrypted connection.

## Configuration

For recurring syncs, use a config file instead of a long flag line. See
[`imapsync.example.yaml`](imapsync.example.yaml). Credentials are always
*referenced*, never stored inline.

```sh
go run ./cmd/imapsync-go probe --config imapsync.yaml --pair icloud-to-mox --side both
```

## Development

```sh
task check       # lint + test
task all         # lint, build, test, completions
```

Or directly:

```sh
go build ./...
go vet ./...
golangci-lint run ./...
go test -race ./...
```

Tests run against go-imap's in-process `imapmemserver`, so no network or
container is needed.

## Releasing

Releases are cut by [GoReleaser] from an annotated tag. Push a `v*` tag and the
`Release` workflow builds binaries for Linux/macOS/Windows on amd64 and arm64,
publishes archives and Linux packages to the GitHub release, and opens a PR
against [hilli/homebrew-tap] with the updated cask.

```sh
task release-check   # validate .goreleaser.yaml
task release-test    # full local snapshot into dist/, nothing published
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

The workflow needs a `PACKAGES_TOKEN` repository secret with write access to
`hilli/homebrew-tap`.

## Licence

MIT

[imapsync]: https://github.com/imapsync/imapsync
[releases page]: https://github.com/hilli/imapsync-go/releases
[Task]: https://taskfile.dev
[GoReleaser]: https://goreleaser.com
[hilli/homebrew-tap]: https://github.com/hilli/homebrew-tap
