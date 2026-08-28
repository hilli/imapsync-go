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

### 4.1 The herd, and why the cooldown needs no clock

Thirty workers meet the wall in the same instant, so thirty refusals arrive. If
each recomputed a width from a falling `open` count they would ratchet the pool
to its floor over a single event. This is the most likely bug in the design.

**While a shrink is still being applied, further refusals are ignored.** The pool
already carries the state that says so — the unpaid token debt from the last
decision (§5). Refusals arriving during that window are the echo of the event
being answered.

Once the pool has actually reached the new width, a further refusal is new
information — another client took connections while we were shrinking — and it
shrinks again. Convergent, and it tracks a limit that moves.

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

**Time needs a seam.** `Options.now func() time.Time`, defaulting to `time.Now`.
One field, and the only way to test the restart case at all: advance past the
window, fail a dial, assert no shrink.

Three failure modes get tests written at them deliberately rather than sampled:

| | |
|---|---|
| Herd | thirty simultaneous refusals shrink **once**. Without §4.1 this collapses to the floor. |
| `Close` after shrink | must not hang. It fails *by hanging*, so it needs its own timeout and a clear message. |
| Idle connections | the connections dropped must be **closed**, asserted on the fake's close count rather than on `Width()`. |

**The barrier, stated rather than hidden:** ten seconds is a judgement call. A
test can prove the mechanism honours whatever the window is; nothing available
here can prove the number is right. The comment in the code says so.

## 8. What a real run has to answer

1. Does the destination pool ever shrink at width 16, or is mox's 30 simply never
   reached in practice?
2. If it shrinks, does it **settle** — or ratchet toward the floor, which would
   mean §4.1's cooldown is wrong or the window is too short?
3. Is ten seconds right?

Growth (the "AI" half of AIMD) is worth building when, and only when, (2)
answers "it ratchets" or a run demonstrably ends far below the width it could
have used.

## 9. Related, separate

`probe` reports *"Suggested concurrency: 47 (one below the ceiling, leaving
headroom for other clients)"* for iCloud, where **no ceiling was found** — 47 is
one below our own cap. `SuggestedConcurrency` does not distinguish "the server
refused at 30" from "we stopped asking at 48". Its own fix and its own commit.
