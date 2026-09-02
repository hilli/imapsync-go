# Duplicate handling — design

Date: 2026-09-02

## Scope

Two of the three things imapsync means by "duplicates":

- **A — duplicates already on the destination.** Delete all but one copy.
  imapsync spells this `--delete2duplicates`.
- **B — duplicates within a source folder.** Copy such a message once rather
  than once per appearance. imapsync does this by default and `--syncduplicates`
  opts out.

**C — the same message appearing in several folders — is dropped.** Gmail's
`\All` is the case that produces it in bulk, and `folder.Role.Virtual()` already
excludes `\All`, `\Flagged` and `\Important` from every run by default. Building
cross-folder detection would mostly be building a detector for artefacts this
tool already refuses to create. Anyone who passes `--include-virtual` has asked
for those copies explicitly, and removing them again would be an odd way to
honour the request.

imapsync's defaults win throughout. This is a drop-in, and a user who knows what
`--syncduplicates` does should not have to find out that we redefined it.

## The key

**SHA-256 digest of the identity headers, plus RFC822.SIZE.**

The digest is `ident.Identity.Digest`, already computed for every message: a
hash over Message-ID, Date, Subject, From, To and Cc. That list is deliberately
short and made only of headers servers do not rewrite, because a header the
destination normalises would make the same message digest differently on the two
sides and duplicate it on the next run.

That same shortness is why the digest alone cannot decide a deletion. Six
headers matching is very strong evidence and not proof — a resent message, a
Message-ID reused by a broken client, a mailing list posting the same
announcement twice with different bodies. Size is a cheap second opinion and,
crucially, a free one: `FetchMeta` already asks for `RFC822.SIZE` on every
message it enumerates, for every folder, today. **Duplicate detection therefore
costs no extra round trips at all.**

### Weak identities are excluded outright

`Identity.Weak` reports that too little header survived for the digest to tell
one message from another. `ident` already says adoption must not act on a weak
identity, because a wrong match silently drops a message instead of copying it.

Deleting on a weak identity is strictly worse than that: it destroys mail rather
than failing to copy it. Weak messages are never grouped, never skipped and
never deleted. They are simply carried, which is the behaviour that loses
nothing.

### Verification before acting — on both halves, not just deletion

Before any message is deleted as a duplicate, the survivor and the victim are
fetched in full and compared. Unequal means not a duplicate, and both stay.

**The same applies to B, which the first draft of this design missed.** Skipping
a copy is not the harmless half: a message that is never copied is as absent
from the destination as one that was deleted from it. The draft treated
verification as something deletion needed and copying did not, which was wrong.

The case is concrete rather than theoretical. `ident` marks an identity weak
only when a message has no Message-ID *and* fewer than two other identity
headers survived, so a message with no Message-ID but an ordinary Subject and
From is fully strong and is digested from five headers. Two automated
notifications sent in the same second — same subject, same sender, same
recipient, same Date — agree on all five, and if their bodies happen to be the
same length the key matches while the mail differs.

B's saving is therefore the upload, not the download: it must fetch the second
body to compare it, but it need not APPEND it. That is still the expensive half
on every real migration.

Digest plus size is a statement about six headers and a number the *server*
supplied — `MessageMeta.Size` is documented in this codebase as a claim rather
than a measurement. Neither destroying mail nor withholding it should rest on
strong evidence when proof is one fetch away. The cost is proportional to the
duplicates found, not to the folder.

## Which copy survives

**The one the state database claims.**

This is not arbitrary. If dedup deletes the copy a `messages` row points at and
keeps an unclaimed one, the row is left dangling — and destination verification,
built the same day, will notice the claim is unhonoured and helpfully copy a
third. The two features would fight, and the visible symptom would be a folder
that grows every time it is cleaned.

Where no copy is claimed, the lowest UID survives, so repeated runs agree.

## Where each half acts

**B** runs during the folder diff, on metadata already in hand. A source UID
whose key has been seen earlier in the same folder is dropped from the copy
list, and the run records that it was skipped as a duplicate rather than copied.

**A** runs against the destination folder. It groups by the same key, verifies,
and deletes with the existing deletion machinery.

The two must talk. B skips source UIDs whose message *is* on the destination
under a different UID, so A must not then treat that destination copy as
unclaimed and remove it. Tracked in-run.

## Limitation, stated rather than papered over

**A folder the CONDSTORE fast path skips is not deduplicated.**

Duplicates arriving on the destination do not move the source's HIGHESTMODSEQ,
so the folder still looks settled — the same shape as the destination-loss bug
fixed in `2026-09-02-destination-verification-design.md`.

The fix used there cannot be reused. That one works because claiming more
messages than a folder holds is *proof* copies are gone. Duplicates make the
destination hold **more** than we claim, and so do messages delivered to it,
filed into it by hand, or left by another tool. There is no sound free trigger,
and a check that fires on every folder holding a stranger is a check nobody
leaves on.

So `--delete2duplicates` cleans the folders a run examines anyway, and the
standalone `dedup` command exists for the rest. Documented in the README, not
left to be discovered.

## Command surface

On `sync`:

- `--sync-duplicates` — opt out of B. Default off, matching imapsync.
- `--delete2duplicates` — enable A. Default off, **but forced on by
  `--delete2`**, as in imapsync: a run mirroring deletions that left duplicates
  behind would be contradicting the request.

Standalone:

```sh
imapsync-go dedup --config imapsync.yaml --pair icloud-to-mox [--folder Sent] [--dry-run] [--force]
```

Destination only. No source connection, no diff, no state write beyond
forgetting rows whose message it removed. It exists because the common case is a
mailbox already made messy by some other tool, where running a full sync is a
slow way to ask a purely local question.

The deletion ceiling is the existing one — 10%, `defaultDeleteFloor`, `--force`
to override. Duplicate deletion is deletion, and a key collision that fired
across a whole folder is exactly the accident that ceiling exists to stop.

## Reporting

Two counters alongside `Missing`, silent at zero:

- `Duplicates` — source messages copied once instead of many times.
- `Removed` — destination messages deleted as duplicates.

## Compat

`internal/compat/table.go` currently refuses four options with "duplicate
handling is not configurable". After this:

- `syncduplicates!` and `delete2duplicates!` become supported.
- `skipcrossduplicates!` and `debugcrossduplicates!` stay unsupported, with an
  honest reason: cross-folder detection was dropped because the virtual folders
  that produce those duplicates are already excluded from every run.

## Testing

- **B, and its near-miss twin.** Two identical source messages produce one copy;
  two messages sharing a subject but differing in body produce two. The first
  test alone is passed by a mutant that collapses everything, and because
  duplicates share a subject, comparing sorted subjects cannot tell success from
  that mutant. The pair can. This is the "counting is not asserting" lesson from
  the destination-verification work, in the one shape where the usual fix does
  not work.
- **The survivor is the claimed copy**, asserted by UID, precisely because the
  alternative interacts with destination verification to grow the folder.
- **A weak identity is never grouped**, however many times it appears.
- **Unequal bytes under an equal key leave both messages alone**, tested at the
  function, since a real SHA-256 collision cannot be crafted.
- **The ceiling fires** on a folder that is entirely duplicates.
- **`dedup` needs no source.** Point it at a config whose source host is
  unroutable. If it completes, the destination-only claim is proven rather than
  asserted.

Live check against `mox localserve`, in a purpose-made folder — never INBOX,
whose junk filter moves appended mail into `Junk` and invalidated an entire live
run during the previous piece of work.

## Build order

1. **The grouping primitive, alone and tested.** Done — `internal/dedup`.
   `Candidates` returns groups of possible duplicates; `Group.Partition` names
   the survivor and the victims together, in one call, so no caller can pair a
   survivor from one rule with victims from another and delete every copy.

   The package is named for what it returns: *candidates*, never conclusions.
   Nothing in it ever sees a body, so nothing in it can confirm a duplicate.

   Three guards keep an unidentifiable message out of every group, and each is
   there because grouping one would lose mail: a weak identity, an empty digest
   — which is `Message`'s zero value, not an exotic case — and a size the server
   never reported, which would silently collapse the key to the digest alone.

   Twelve mutations, all caught. The one that first survived was the loss of
   the within-group sort, because every test fed UIDs in ascending order and
   the sort had nothing to do. iCloud's `UID SEARCH ALL` returns them unsorted,
   so the test now does too.
2. B — skip source duplicates, verifying before the skip.
3. A — delete destination duplicates.
4. The `dedup` command.
5. Compat wiring and documentation.
