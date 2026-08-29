# Connection governor — design

**Date:** 2026-08-28
**Status:** designed, not yet built
**Supersedes:** §4.2 of [the main design](2026-08-27-imapsync-go-design.md)

## 1. Why this exists, and why the original reason was wrong

§4.2 specified a per-side AIMD governor and justified it like this:

> Expected settling points: iCloud 2–5, mox 30+. The governor exists primarily
> because iCloud throttles aggressively and unpredictably.

It was deferred at M3 on the grounds that no throttling had been observed, and a
controller tuned against a server that is not pushing back is a controller tuned
against nothing. Measuring both servers has now overturned the premise rather
than confirming it.

| | §4.2 predicted | Measured, 2026-08-28 |
|---|---|---|
| iCloud (source) | throttles aggressively, settles 2–5 | **≥48; never refused** |
| mox (destination) | 30+ | **exactly 30, a hard wall** |

`probe --max-connections 48` against iCloud reached the configured cap without a
refusal. The same probe against mox stopped at 30 with
`authenticating as ...: unexpected EOF`.

So the side that pushes back is the **destination**, and it does so with a
stable, server-configured count rather than an unpredictable throttle. Both
halves of §4.2's reasoning — which side, and what kind of limit — were wrong.

## 2. The bug the measurement found

mox refuses a connection past its limit by **hanging up during
authentication**. That surfaces as `unexpected EOF`, which
`retry.classifyNetwork` maps to `Again`:

```go
// An unexpected EOF is a connection closed mid-response; a plain EOF is one
// closed between them. Both mean the peer went away, which a new connection
// fixes.
if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
	return Again, true
}
```

`Again` means *retry promptly on a fresh connection*. So the one connection-limit
refusal anybody has observed on a real server is answered by immediately dialling
into the same wall, at the same width, by every worker at once — which is
precisely the failure `retry.go` already warns about two hundred lines earlier:

> a server that says "too many connections" is asking for less load, and
> answering it with a prompt retry is how a throttle becomes a ban.

The classifier is not at fault. **A connection-limit refusal and a dropped TCP
connection are byte-identical**, and no amount of looking harder at the error
separates them. Only context does.

This is a better problem than the one the governor was built for, because it is
real, observed, and reachable in a run that uses today's default widths.

## 3. Telling a wall from a blip

`retry.Classify` is a pure function of an error and should stay one. The pool
knows the thing the classifier cannot: whether other connections are working.

A failed dial is wrapped:

```go
fmt.Errorf("dialling: %w", errors.Join(pool.ErrAtCapacity, err))
```

and `Classify` gains one rule: `ErrAtCapacity` → `Slower`. Nothing else about
the classifier changes.

**The rule for attaching it.** A dial failure is a capacity refusal *if and only
if another connection completed work successfully within the last ten seconds.*

That condition is the design, not a refinement of it. Without it a **server
restart** is a false positive: every connection dies at once, every worker
reconnects, every dial fails, and a governor that only ever shrinks walks the
pool down to its floor over a fault that has nothing to do with counting. With
it, a restart produces no recent successes and no shrink, while a genuine wall
always has twenty-nine other connections busy proving the server is alive.

The window must outlast one slow operation — a 30 MB fetch over a bad link —
without outlasting a restart. **Ten seconds is a judgement call and no test can
validate it**; see §7.

The deliberate consequence: a dial failure with no recent success stays `Again`.
Missing a real wall costs a retry. Shrinking on a restart costs the rest of the
run.

## 4. How far to shrink

§4.2 specified multiplicative decrease. That is the right tool when the limit is
invisible and congestion is being inferred from loss — you halve because you have
no idea what the number is.

The measurement says the limit is neither invisible nor unstable, and at the
moment of refusal **we already know it**: the number of connections currently
open is exactly the number that works. Halving 16 to 8 when the answer is 15
discards half the run's throughput to discover something already in hand.

**New width = connections open at the moment of refusal.** One step, no
overshoot, nothing to oscillate. Floor of 1. It never grows again (§6).

### 4.1 The herd, and the cooldown that was not needed

Thirty workers meet the wall in the same instant, so thirty refusals arrive. If
each recomputed a width from a falling `open` count they would ratchet the pool
to its floor over a single event. This was the most likely bug in the design,
and it was designed against directly: a cooldown suppressed refusals while a
shrink was still being applied, using the unpaid token debt (§5) as the flag
that said so.

**The cooldown was built, and then deleted.** Mutation testing removed it and no
test failed. Working out why showed it was not merely redundant but wrong.

The herd is already handled by the arithmetic. All thirty refusals read the same
`open` count, so all thirty compute the same new width, and the guard `to >=
width` means twenty-nine of them find the pool already where they would have put
it. The protection is a consequence of shrinking to an observed number rather
than a machine we added; halving would have needed the cooldown, which is
probably why the design started with both.

The only case where the cooldown changed the outcome was a connection breaking
while the pool still owed tokens — `debt > 0 && open < width`. There, shrinking
to `open` is the correct answer, and the cooldown withheld it.

This is worth recording as a method result: the mutation did not find a bug in
the code, it found machinery the code did not need. Removing it left the
behaviour identical everywhere the tests could see and better in the one place
they could not.

Once the pool has reached the new width, a further refusal is new information —
another client took connections while we were shrinking — and it shrinks again.
Convergent, and it tracks a limit that moves.

The refusing operation is unaffected: it retries as `Slower` under the existing
exponential-with-jitter policy, so no message is dropped by any of this.

## 5. The mechanism

The pool holds two resources, deliberately decoupled: a token is required to
*use* a connection, but an idle connection holds no token. **Shrinking must
reduce both.** Cutting tokens alone would leave idle connections open and still
counted by the server — the shrink would look like it worked and change nothing
on the wire.

**Token debt.** `shrink(to)` records `debt += width - to` and sets `width = to`.
Tokens are then destroyed two ways:

- *immediately*, by draining whatever is free, non-blocking;
- *lazily*, in `put`: while `debt > 0` a returning connection is **closed** and
  its token is **not** returned. One release pays one token and one connection.

**Connections** are reduced against the same target independently: at shrink
time, idle connections are closed until `open <= width`, as far as the idle stack
allows. The rest go as leases come back.

The invariant at `pool.go:60` — *"capacity can be neither leaked nor invented"* —
survives in a stricter form:

```
width == cap - swallowed
```

with `swallowed` rising only by the amount `shrink` recorded. Capacity still
cannot be invented; it can now be destroyed, in one place, by one function.

**`Close` has to change.** It drains `p.cap` tokens to prove every lease is
finished; swallowed tokens never come back, so it would hang for ever. `shrink`
becomes a no-op once `closed` is set, so once `Close` has taken the mutex and set
the flag, `swallowed` is frozen and it drains `cap - swallowed`.

**New surface:** `Pool.Width()` beside `Cap()`.

## 6. Scope

**Cut: growth.** Additive increase is not built. The wall is a stable count, so a
run that finds it should sit under it. The cost of the omission is stated plainly
because it is real: **shrinking is permanent for the life of a run**, so a one-off
crowding at minute three of a nine-hour run costs the remaining nine hours. §8
lists what a real run must show before growth is worth building.

**Cut: per-host width remembered across runs.** §4.2's "per-side, per-host"
implies persistence. It is wrong as well as extra — the wall is *shared*, so
yesterday's settled width describes yesterday's other clients. `probe` measures
the real number on demand.

**Cut: a flag for the window, and a flag to disable shrinking.** Nobody can tune
what they cannot observe, and `--connections` already caps the thing. A
misfire in the wild is evidence for a flag; guessing now is how a knob nobody
understands gets supported for ever.

**Kept: both pools.** They are the same type, so the source gets it free. iCloud
showed no wall at 48, but "no wall found" is not "no wall".

**Kept, and it is the point: report the settled width.** A log line per shrink
and the final width in the run summary. Without it, this design is untestable in
the field and the question of growth goes back to folklore.

## 7. Testing

**The centrepiece is a fake wall.** A `DialFunc` that counts live connections and
returns `io.ErrUnexpectedEOF` past a threshold is, for our purposes, mox. The
test asserts the pool converges to the threshold and then **stops dialling past
it** — the behaviour that matters, and the one a counter-only assertion misses.

**Time needs a seam.** A `now func() time.Time` on the pool, defaulting to
`time.Now`. It is the only way to test the restart case at all: advance past the
window, fail a dial, assert no shrink.

It sits on `Pool` rather than on `Options` because `pool_test.go` is an external
test package and could not have set an unexported option. Exporting it to make
the tests convenient would have put a clock in the public API for no caller's
benefit, so the clock tests live in-package instead.

Three failure modes get tests written at them deliberately rather than sampled:

| | |
|---|---|
| Herd | thirty simultaneous refusals shrink **once**, by the `to >= width` guard alone (§4.1). |
| `Close` after shrink | must not hang. It fails *by hanging*, so it needs its own timeout and a clear message. |
| Idle connections | the connections dropped must be **closed**, asserted on the fake's close count rather than on `Width()`. |

**The barrier, stated rather than hidden:** ten seconds is a judgement call. A
test can prove the mechanism honours whatever the window is; nothing available
here can prove the number is right. The comment in the code says so.

## 8. What a real run answered

Run against mox on 2026-08-28: a pool capped at 40 against a server that holds
30, forty workers, twelve leases each.

1. **Does it ever shrink?** Yes. One shrink, 40 → 23, then nothing.
2. **Does it settle or ratchet?** It settles. 470 leases succeeded and 10 failed,
   all ten in the opening burst before the pool knew where the wall was.
3. **Is ten seconds right?** Still unanswered, and now much less load-bearing —
   see below.

The run also found two bugs that every unit test had passed, both because the
tests primed the pool before pushing on it and a real run does not.

**The signal arrived too late.** A successful *dial* was not evidence; only a
completed lease was. But every worker dials at once, so the first refusals land
before anybody has finished anything, and were judged against nothing. The pool
never narrowed at all: 120 failed leases, no shrink, for the whole run. A dial
that succeeds is stronger evidence than work that finished — it is the server
accepting a connection, which is precisely the question — and it is available at
the moment the refusals arrive. This also makes the ten-second window far less
important, because the evidence now arrives in the same instant as the thing it
is judging.

**The tokens went round again.** A shrink cannot reclaim tokens already lent
out, so it records what it is owed and collects them as they return — but only
`put` paid, and `put` is the path a *successful* lease takes. The workers holding
the excess tokens were the refused ones, and they handed theirs straight back.
So the pool held a small width and went on lending concurrency it had already
decided it could not have: 121 failed leases against 10, and the version that
failed twelve times more was the one that settled on the *wider* number, which is
how the bug stayed invisible. Every path that returns a token now pays.

Both are worth recording as a method result: mutation testing found the machinery
that was not needed, and only a real server found the machinery that was missing.

The settled width of 23 is short of mox's true 30, because a shrink lands on what
is demonstrably open at that instant rather than on the maximum. It never grows
back, so that is a real cost, and it is the strongest argument yet for the
increase half of AIMD. It is still not worth building on one measurement: the
run is printed now, so the next few will say whether 23-of-30 is typical or was
an artefact of forty workers starting in the same millisecond.

## 8.1 What the full-scale run answered

Run against the account the tool exists for, 2026-08-29: iCloud → mox, 776,791
messages across 141 folders, both pools on `auto`. 2h49m1s. 52,591 copied,
643,481 adopted, 2 folders failed. Peak RSS 104 MB against a 256 MiB limit.

The four questions §8 set, answered honestly:

1. **Does the destination pool shrink at a realistic width?** **Unknown, and the
   run could not tell us.** Zero shrinks were logged — but the pool logs nothing
   about its width either, so "never shrank" and "shrank silently" are not
   distinguishable from the outside. `OnShrink` logs a shrink; nothing logs the
   settled width, the cap, or the starting point.
2. **Does it settle or ratchet?** Unanswerable for the same reason.
3. **Is the settled width consistently short of the true limit?** Unanswerable.
   The case for the increase half of AIMD is exactly where §8 left it, and one
   more run has not moved it.
4. **Does anything degrade non-linearly between 135 and 776,791 messages?**
   Memory, no: 104 MB peak, flat. Connections, **yes** — see below.

**The governor is unobservable, and that is the finding.** Three of the four
questions this design set for itself cannot be answered by the largest run the
tool has ever done, because the thing being asked about is silent. A measurement
you cannot read is not a measurement. Width belongs in the run's log at the
point it settles, and until it is there, every future run wastes the same
opportunity this one did.

### What did degrade at scale: idle connections

The two largest folders both failed, and with the same error:

    acquiring destination connection: selecting "INBOX":
    use of closed network connection

INBOX (413,934) and Reklamer (96,147) between them left 68,755 messages
uncopied. All 139 shorter folders finished.

This is non-linear in exactly the way question 4 was asking about, but not in a
dimension the question anticipated. It is not that big folders move more data;
it is that they *take longer*, so the pool's spare connections sit idle longer,
and mox hangs up on a connection that sits. The failure scales with a folder's
duration, not its size, and nothing below about half an hour reached it.

`ready` then reported the dead socket as the folder's failure, which named the
wrong thing entirely: the mailbox was fine and the server was fine. Fixed by
spending one fresh dial on a reused connection whose SELECT fails, and never on
a freshly dialled one — a connection just opened has proven the server is there,
so a SELECT it refuses is the server refusing.

The same path had been leaking its open count all along: a failed SELECT closed
the connection without uncounting it, and once `open` passes `width`, `put`
stops idling returned connections and closes them. A pool that met a few stale
connections quietly stopped pooling.

**Verified by re-running the same account**, 2h1m43s, 68,314 copied at 9.4/s,
**zero folder failures**. INBOX and Reklamer both completed — 413,938 and 96,147
— and the errors that had killed them arrived again and were absorbed:

    WARN destination search failed: searching for header Message-ID:
         use of closed network connection
    WARN retrying append: Reklamer: appending message 69551: declared
         37405 bytes but sent 0: connection desynchronised and closed

Same server, same folders, same failures, now warnings instead of an abandoned
folder. Every folder reconciles: 776,802 source messages against 776,807
accounted, the five extra being advertising that arrived in Reklamer while the
run was reading it.

The second run also measured what the state database is worth: 139 of 141
folders were settled in the first sixty seconds, because they were already done.

Peak RSS was 239 MB against the 512 MiB in-flight budget, against 104 MB on the
adoption-heavy first run — memory tracks what is being copied, not how many
messages are being considered, which is the right shape.

### Two folder-level observations worth keeping

- **A failed folder is retried, and the retry works.** INBOX failed at 18:05 on
  a mox EOF during destination enumeration, was retried at 19:00 and got all the
  way through diffing 413,934 messages. Folder-level retry is earning its place.
- **The completed counter passed its own denominator** — `folders: 142/141` —
  because a retried folder increments it twice. Cosmetic, but it is the kind of
  thing that makes a progress line untrustworthy.

### The rate figure measures the wrong thing

The run reported **5.2 messages/second**. It actually processed 696,072 messages
in 10,141 seconds, which is 68.6/second. `rate` counts copies and ignores
adoptions, so the pass this design recommended running *first* — the adoption
pass — is the pass whose progress line understates it thirteenfold. An operator
watching "5.2 msg/s" against 776,791 messages would reasonably conclude it had
hung.

### Two messages the destination refused

    imap: NO APPEND delivering message: generating preview:
    making preview from text part: bufio.Scanner: token too long

A mox bug, not ours, and correctly reported as two named messages rather than a
failed folder. Recorded because a drop-in replacement will meet servers that
reject individual messages, and "2 failed, here are their UIDs" is the right
shape for that.

### Source enumeration could not be believed

Found while preparing this run and fixed before it: iCloud's SEARCH index
retains expunged messages, so `SEARCH ALL` answered 100,184 UIDs for a 487
message Trash and 503,763 for a 413,933 message INBOX. 189,526 phantoms would
have been fetched, come back empty, and been written to the state database as
gone — a fifth of the run, every run. `AllUIDs` now checks its answer against
EXISTS and walks the mailbox when the two disagree.

## 9. Related, separate — fixed

`probe` reported *"Suggested concurrency: 47 (one below the ceiling, leaving
headroom for other clients)"* for iCloud, where **no ceiling was found** — 47 was
one below our own cap. `SuggestedConcurrency` could not distinguish "the server
refused at 30" from "we stopped asking at 48", so the more we asked for, the
lower its advice went, always by exactly one.

`Report.Refused` now records which of the two ended the search. Where the server
refused, the advice is unchanged: stay one below the wall so a competing client
does not push the sync over it. Where the search only ran out of its own budget
there is nothing to stay below, so the suggestion is the count itself and the
output stops calling it a ceiling — a number we chose should not be written into
a config as though it had been measured.

Its own commit.
