# Destination verification — design

Date: 2026-09-02

## The problem, measured

A destination copy deleted by anything other than this tool was lost for ever.
The state database recorded the source UID as done, `triage` turned that into
`AlreadyDone` and dropped the UID, and nothing ever asked the destination
whether the claim was still true.

Measured against a real server before writing any code:

```
first run:          copied=2         destination holds 2
external delete:                     destination holds 1
second run:         copied=0         destination holds 1   <- lost
third run (--full): copied=0         destination holds 1   <- --full does not help
```

`--full` looked like the answer and is not. It bypasses the CONDSTORE
watermark, but what it then re-diffs is the *source*. A source that has not
changed produces the same answer however thoroughly it is examined.

This had been written off in a code comment as "a real gap rather than an
oversight, and `--full` is its answer". That sentence was the bug, and it is
the third time in this project that a test or comment asserting a premise has
turned out to be asserting the defect. It also contradicted a stated
expectation of this tool: that files may change around the database without the
database being told.

## Design

### Trigger: free, and sound in one direction only

`state.ClaimedCount` is the number of messages the database claims to have put
in a folder. If that exceeds the destination's message count, copies are
provably gone: we cannot have put 5 messages in a folder holding 3.

The reverse proves nothing. A destination legitimately holds messages we never
copied — mail delivered to it, mail from an earlier tool, mail the user filed
by hand — so a claim count at or below the message count is consistent with any
amount of loss. The check is therefore an under-approximation by construction,
and that is the correct direction: it costs nothing, never fires falsely, and
catches the case that matters (wholesale loss) while missing the case that is
indistinguishable from a healthy folder without asking.

Cost: zero round trips. The count arrives with the folder listing.

`ClaimedCount` is a `COUNT(*)` rather than `len(Mirrored())`. Reading 400,000
rows into memory to take their length is exactly the mistake that cost 19 MB in
`resyncFlags`, and this runs once per folder.

### Verification: paid, and only when the trigger fires

`dst.AllUIDs` into a set, walk `Mirrored`, and `ForgetMessages` every source UID
whose recorded destination UID is absent. `ForgetMessages` already existed as
the release-the-claim primitive.

No new copy machinery. Forgetting a row puts the source UID back in front of
`triage`, which schedules it in the same run by the ordinary path.

### The fast path was the whole problem

The first implementation passed five unit tests and did nothing at all against
a real server. The default run reported "folder unchanged" and copied nothing.

The CONDSTORE fast path returns before any destination check, and it fires
*exactly* when destination-side loss happens: deleting from the destination
moves nothing on the source, so the source HIGHESTMODSEQ is unchanged and the
folder looks untouched. **The check was most confidently skipped in precisely
the situation it existed to catch.**

The destination LIST now carries STATUS — `imapx.ListOptions.WithStatus` and
LIST-STATUS support already existed, used only by `probe` — and `canSkip`
consults the claim count before allowing the skip. On a LIST-STATUS server this
is free, because the counts arrive in the listing already being fetched. On
others it is one STATUS per destination folder.

Deliberately skipped:

- `!kept` — the UIDVALIDITY fence has already invalidated every row.
- A server that will not answer STATUS. Reading silence as "possibly missing"
  would send every folder down the slow path on such a server, which is the
  opposite of a check whose justification is that it is free.

### Watermark

`Missing` does **not** suppress the CONDSTORE watermark, unlike `Filtered`. A
message deliberately filtered out is not on the destination and the watermark
must not claim the folder is settled. A re-copied message genuinely is on the
destination by the end of the run, so the watermark is honest.

### Flag and reporting

`--verify-dest`, default true, opt-out. Dry runs verify and report but repair
nothing. The report names the count and says plainly that something else
removed the messages, because a user who did not expect this needs to know why
mail reappeared.

## Behaviour change worth stating

A message the user *moved* out of the destination comes back. A move is a
delete plus an append elsewhere, and we only ever see the delete. Perl imapsync
behaves the same way. It will surprise someone.

## Testing

Five slow-path tests, three fast-path tests, one state-scoping test, four
report-wording tests.

Mutation results: verification guards 10/10 caught; fast-path guards 5/6, the
sixth documented as a deliberate barrier; wording 3/3. Honest controls survived
in every run.

Three findings from the mutation work:

- **Counting is not asserting.** An inverted-presence mutant survived because it
  forgot the surviving claim, re-copied it, and left the destination holding two
  messages — the same count as success, with one message still lost and another
  duplicated. Assertions now compare the sorted multiset of subjects.
- **Redundant enforcement masks mutations.** `NoVerifyDest` is enforced twice —
  the listing does not fetch status, so counts arrive nil and `canSkip` returns
  early anyway — so each mutation is masked by the other. Kept as a documented
  barrier, on the `p.skipped` precedent in `deleteVanished`, and covered live
  instead.
- **The report wording had been wrong twice**, most recently "and *them* have
  been copied again" from reusing one pronoun variable in both the object and
  subject slots. Now tested. A report that cannot write a sentence correctly is
  one a reader stops trusting about its numbers.

### The unit suite could not see any of this

`rev1Caps`/`rev2Caps` advertise no CONDSTORE and `imapmemserver` implements none,
so the in-memory suite cannot reach the fast path at all. Five green tests stood
over an engine that was completely inert against a real server. The fast-path
tests use the connection-decorator harness already in `condstore_test.go`
(`modseqSource`/`modseq`) — found after starting to build a TCP proxy for the
job, and much simpler.

## Live verification (mox localserve)

Source `Live`, destination `Mirror`, four messages copied, then two destination
copies deleted externally and one new source message appended:

```
WARN destination is missing copies this run recorded; missing=2
Live  Mirror  5  copied=3  already=2  failed=0
```

Both sides then held exactly `[five four one three two]` — no duplicates. A
following run logged "folder unchanged" at zero cost. A dry run reported the
loss and changed nothing, and the real run after it repaired. With
`--verify-dest=false` the folder was declared unchanged and nothing was
repaired, which is the live evidence the masked unit mutations could not give.

**Gotcha for future live tests:** mox's junk filter moves IMAP-appended messages
out of `Inbox` into `Junk`, and mox spells the mailbox `Inbox`, not `INBOX`. An
earlier run of this scenario appeared to restore only one of two missing copies;
the missing messages had been reclassified into `Junk` and were genuinely no
longer in the source. The tool was correct and the test environment was not.
