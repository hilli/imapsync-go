# Rate limiting

## 1. Why

This tool exists to go fast. It has no brake.

The only throttle available today is `--source-connections` / `--dest-connections`,
and a connection count is the wrong unit for the job in both directions: one
connection on a fast link can saturate a household's uplink, and forty
connections against a folder of small messages may move almost nothing. Lowering
the width to slow a transfer down also lowers the parallelism that makes the
tool worth using, so the two things a user wants to control separately can only
be controlled together.

Three situations want a brake, and none of them is exotic:

- **A shared or metered link.** A 776,802-message migration ran for two hours.
  Anyone else on that connection had two hours of it.
- **A provider that watches the rate.** This is the sharp one. A provider that
  decides a client is abusing the account does not usually respond by slowing it
  down; it responds by locking the account. Being fast is exactly the behaviour
  that trips it.
- **A small server.** A self-hosted destination on modest hardware is not
  helped by being asked for everything at once.

imapsync has had `--maxbytespersecond` and `--maxmessagespersecond` for years,
and `compat` refuses both. Their refusal text — *"rate limiting is not
implemented; reduce --source-connections and --dest-connections instead"* —
recommends the coarse control above as a substitute for the fine one, which is
the best that could be said and is not much.

## 2. What imapsync counts, and what we will count

imapsync's limit is on **message bytes**: the sum of the sizes of the messages
copied. It does not count protocol overhead, TLS, header fetches or SEARCH
responses. Confirmed against imapsync's own documentation rather than assumed,
because the number has to mean the same thing here for the option to be a
drop-in.

We count the same thing, for two reasons beyond compatibility. Counting wire
bytes would mean wrapping every connection in a metering reader and writer, and
it would still not produce imapsync's number — so the option would accept
imapsync's flag and quietly mean something else, which is worse than refusing
it. And message bytes are the number a user can predict: a 40 GB mailbox at
2 MiB/s is about six hours, and that arithmetic is the reason anyone sets the
flag.

**The one thing this makes non-obvious has to be said in the help text rather
than left to be discovered.** A message crosses the wire twice — down from the
source and up to the destination — and is charged once. `--max-bytes-per-second
1MiB` therefore produces roughly 2 MiB/s of actual network traffic, split
between two hosts. imapsync has the same property and does not mention it.

## 3. The design point: concurrency changes what a rate limit is

imapsync is sequential. One connection, one message at a time, so "work out how
long this message should have taken and sleep the difference" **is** a rate
limit. That implementation is correct there and is wrong here, in a way that
would not show up in any test written against a single worker.

This tool runs up to forty fetchers, across several folders at once. Forty
workers each sleeping the same computed interval produce forty times the
requested rate. The naive port of imapsync's approach does not merely
approximate the limit — it misses by a factor equal to the concurrency, and the
faster the machine is configured the further off it is.

So the limit is **one allowance shared by the whole run**, held by the `Syncer`
and consulted by every worker, rather than a delay applied per worker. A token
bucket: tokens accrue at the configured rate, a worker takes what its message
costs and waits if they are not there yet.

This is the property worth a test of its own, because it is the one a
plausible-looking implementation gets wrong. See §7.

## 4. Where the charge is made

The copy pipeline is: fetch body → `pending` channel → append.

The charge goes at **the fetch site**, in `fetchOne`, immediately before
`src.FetchBody` — beside the byte budget, which is charged in the same place for
a different reason. That single point throttles the entire run, because the
appenders can only append what the fetchers have produced. The `pending` channel
is buffered at `dst.Cap()`, so the append side can run ahead by a bounded burst,
but it cannot sustain a rate the fetch side is not feeding it.

One charge point rather than two follows from that, and it settles what the
number means:

| Case | Charged? | Why |
|---|---|---|
| A message copied | yes | its bytes crossed the wire |
| A message **adopted** | no | no body was fetched; a re-run is not throttled |
| A message skipped as a duplicate | yes | the body was fetched before the comparison |
| A message filtered out | no | nothing was fetched |
| Header and metadata fetches | **no** | see below |
| A dry run | no | nothing is fetched |

The header sweep is the honest gap. On a 400,000-message folder it is real
traffic, and it is not counted. imapsync does not count it either, so counting
it would break the drop-in equivalence to fix an under-count that is a few per
cent of a copying run — and 100% of a run that copies nothing, where there is
nothing to throttle anyway. Recorded here rather than papered over.

The size charged is `meta.Size`, the server's declared `RFC822.SIZE` — the same
figure the byte budget uses, and charged *before* the read rather than after,
because a limiter that waits after spending the bandwidth is not a limiter.

## 5. Implementation

A new package, `internal/throttle`, holding two `golang.org/x/time/rate`
limiters — one counted in bytes, one in messages — behind a single `Wait`.

`golang.org/x/time` is a new dependency. It is a `golang.org/x` module, the same
trust level as `golang.org/x/sync`, which is already here for `errgroup` and
`semaphore`; it has no transitive dependencies. The alternative is hand-rolling
a token bucket, and the parts of that which are easy to get wrong — reservation
ordering, a clock that moves backwards, cancellation while holding a
reservation — are exactly the parts `x/time/rate` has already got right.

**`nil` is a working limiter that charges nothing**, matching `*budget.Budget`.
That convention is what keeps `if s.throttle != nil` out of the copy path.

### The burst problem

`rate.Limiter.WaitN` refuses outright when `n` exceeds the burst size, so a
30 MB message against a 1 MiB/s limit is an error rather than a wait. Two ways
out, and the choice matters:

- Clamp the charge to the burst. Simple, and **wrong in the dangerous
  direction**: a run of large messages would be systematically under-charged and
  would exceed the limit the user set.
- Charge in burst-sized instalments. A 30 MB message at 1 MiB/s becomes thirty
  waits of about a second, totalling thirty seconds — which is the correct
  answer.

Instalments, and they cost about five lines. They have a second benefit that was
not the reason for choosing them: because a large message re-queues between
instalments, it does not block the head of the line, so small messages
interleave with it instead of waiting out its whole transfer.

Burst is one second's worth of the limit. Enough to absorb the ordinary jitter
of forty workers arriving together, small enough that the opening burst is not a
visible spike.

## 6. Surface

### Flags

```
--max-bytes-per-second 2MiB      # accepts the same sizes as --max-size
--max-messages-per-second 20     # may be fractional
```

Spelled out rather than shortened to `--max-rate`, because the unit is the
whole question and this codebase spells things out (`--dest-connections`, not
`--dest-conns`). They also read as near-transliterations of imapsync's names,
which is what someone converting a command line is looking for.

### Config

```yaml
concurrency:
  source: 40
  dest: 24
  max_inflight: 512MiB
  max_bytes_per_second: 2MiB
  max_messages_per_second: 20
```

Under `concurrency:` because it answers the same question as the rest of that
block — how hard to push — and a user looking for a brake will look where the
accelerator is.

Precedence is flag, then config, then unlimited, decided with `Changed()` rather
than a zero sentinel, consistent with the widths and `--delete2`.

**`concurrency:` is the block that has already silently dropped two of its
fields**, once costing the largest run this tool has ever done its configured
widths, and once disarming `delete2:`. Adding two more fields to precisely that
struct without doing something about the pattern would be negligent, so §7
includes a structural test that fails when a field of `config.Concurrency` is
not consumed.

### Report

The report says the limit whenever one is set, and how much time the run spent
waiting on it.

The wait time is the point. Without it a throttled run is indistinguishable from
a slow server, and the user cannot tell whether their own brake or the network
is the constraint. This is the lesson from the connection note, which for two
full runs spoke only when a pool *shrank* and therefore said nothing at all
about the case that actually held — leaving three of the governor design's
questions unanswerable.

### compat

`--maxbytespersecond` and `--maxmessagespersecond` become translations.

Two refusals carry the reason *"rate limiting is not implemented"*, which this
change makes false:

- `--maxsleep` stays refused, with a true reason: it caps the length of the
  sleep imapsync inserts between messages, and a token bucket has no such sleep
  to cap.
- `--maxbytesafter` stays refused, with a true reason: it asks for the limit to
  begin only after a number of bytes has moved, which is a warm-up this tool
  does not have.

`--exitwhenover` stays refused and its reason stays true. It is a quota rather
than a rate — stop the run after N bytes — and although the counter this change
adds would make it cheap, it needs a "stop early and report success" exit path
that does not exist. Nobody has asked for it. YAGNI.

## 7. Testing

Ordinary unit coverage of the limiter (rate honoured, nil-safe, instalments,
cancellation), plus three that carry weight:

1. **The allowance is shared, not per worker.** N workers charging concurrently
   must together not exceed the rate. This is the test that fails against the
   naive port of imapsync's design, and it is the reason the package exists in
   this shape.

2. **Timing assertions are lower bounds only.** Moving X bytes under a limit of
   R must take at least X/R, less one burst. An upper bound would be a
   wall-clock assertion on a loaded machine, which this project has already had
   to remove once.

   The accounting is tested separately and deterministically — what was charged,
   in how many instalments — so the parts that can be checked exactly are not
   left to the clock.

3. **Every field of `config.Concurrency` is consumed.** By reflection over the
   struct against the source of the function that resolves it. This is the test
   that would have caught both of the bugs described in §6, and its value is
   that it fails when someone adds a sixth field, which is the moment the
   knowledge is lost.

Then mutation testing over the limiter and the wiring, per the practice
everywhere else in this project.

## 8. What this deliberately does not do

- **No wire-byte accounting.** §2.
- **No separate source and destination limits.** One allowance, charged once.
  Two numbers would have to be explained in terms of a pipeline the user cannot
  see, and the fetch side already governs the append side.
- **No adaptive rate.** The connection governor exists because a server's
  connection ceiling is a fact to be discovered. A rate limit is not a
  discovery; it is a user's declared ceiling, and a tool that quietly exceeded
  it because the server seemed happy would be doing the one thing the flag was
  set to prevent.
- **No `--exitwhenover` quota.** §6.

## 9. Build order

1. `internal/throttle`, with its tests. Nothing else depends on it yet.
2. Charge it in `fetchOne`; thread it through `syncer.New` beside the budget.
3. Flags, config, precedence, and the structural test over `config.Concurrency`.
4. The report note.
5. `compat` translations, the two corrected refusals, README, generated doc.
6. Mutation testing, then a live run against mox with a limit low enough to bind.

All six are done; §10 records what each one taught.

## 10. What building it taught

All six steps are done. Five things were learned that the design did not
anticipate, four of them by a test or a mutation and the fifth by a live run.

### The retry table would have turned a deadline into 400,000 failures

`x/time/rate` declines up front when it can see a wait will outlast the
context's deadline, and reports that in an error of its own —
`"rate: Wait(n=1) would exceed context deadline"`, wrapping nothing.

`retry.Classify` asks first whether an error is a context error, and falls
through to `Skip` for anything it does not recognise. `fetchChunk` turns `Skip`
into "record this message as failed and take the next one". So a throttled run
that hit its deadline would not have stopped: it would have written off every
remaining message, one refusal at a time, and reported a few hundred thousand
failures where it should have reported one cancellation.

`classifiable` wraps `context.DeadlineExceeded` around such an error, and only
when the context has a deadline and the error is not already a context error.
The test asserts on `retry.Classify(err) == retry.Stop` — the consequence —
rather than on the error's shape.

The path is unreachable today, because the sync context carries no deadline.
Three lines, kept because the trap is the kind that is laid now and sprung by
an unrelated change later.

### A report number the flag describing it would not accept

The first `humanBytes` rendered 1500 bytes as `1.465KiB`, which is a fair
description and is rejected by `--max-bytes-per-second`. The number in the
report is the number someone pastes into their next command line.

Split in two: `flagBytes` steps down to a unit that divides evenly and falls
back to plain bytes, so it round-trips exactly; `humanBytes` stays approximate
and is used for the volume moved, which nobody pastes anywhere.

The first version of the test asserted this of `flagBytes` alone, and a mutation
swapping the report's call site to `humanBytes` survived it. The test now goes
through the note.

### Three mutations found three real gaps

Sixteen mutations, five survivals, all closed by a new test rather than by an
argument:

- **The byte allowance was charged `meta.Size` and nothing said so.** Both CLI
  rate tests used the message limit, so replacing the size with `0` changed
  nothing they could see. Same shape as the bug that left `concurrency:` and
  `delete2:` inert for the life of the tool: a value parsed, validated, carried
  most of the way, and then quietly not used.
- **A Ctrl-C could be reported as a timeout.** Removing `classifiable`'s first
  guard changes nothing about what the run *does* — `retry.Classify` reads a
  wrapped `context.Canceled` as `Stop` either way — but it changes what the
  person who interrupted a long migration is told.
- **A negative byte size was accepted when written without a unit.**
  `ParseByteSize` refused `-1MiB` and accepted a bare `-1`, so the spelling
  needing least thought was the one that got through, and `max_inflight: -1`
  went with it. Fixed in `ParseByteSize` rather than per-field, which made the
  `max_bytes_per_second` check in `Validate` unreachable, so it was removed.

### The live run found a bug no unit test was looking for

Twenty messages of four kilobytes, held to five messages a second, reported
**"having moved 0B of message data"**. The volume was counted inside the branch
that consults the byte allowance, so a run limited only by message rate counted
nothing.

No mail was at risk. What was at risk is the only thing the note exists for:
answering whether the brake is what is holding the run up, and at what cost,
from a note that described a real transfer as nothing at all. The volume is now
counted for every message that passes through, which is what the sentence
around it already claimed.

### Live results

Against `mox localserve`, twenty messages of about 4 KiB in a purpose-made
folder:

| Run | Time | Note |
| --- | --- | --- |
| No limit | 1.009s | baseline |
| `--max-bytes-per-second 16KiB` | 4.161s | waited 3.935s, moved 80.73KiB |
| `--max-messages-per-second 5` | 3.367s | waited 2.789s, moved 80.73KiB |
| `--full` re-run, byte limit still set | 43ms | nothing ever waited |
| Fresh state, 20 adopted, byte limit set | 44ms | nothing ever waited |

The predicted times are `(82668 - 16384) / 16384 = 4.05s` and `(20 - 5) / 5 =
3s`. The last two rows are the property that matters most: the run that settles
776,802 messages in 1m27s is not throttled to the copy rate for copying nothing.
