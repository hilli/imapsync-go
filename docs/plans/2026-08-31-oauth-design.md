# OAuth — XOAUTH2, and a credential that can be asked twice

## 1. Purpose

Microsoft completed the retirement of IMAP basic authentication for Exchange
Online in April 2026. As of today this tool cannot connect to a Microsoft 365
mailbox at all, by any spelling of any option. Google still permits app
passwords where two-factor authentication is on, but has been pushing the same
direction since 2022.

Migrating *off* Office 365 is one of the most common things anyone uses
imapsync for, so this is not an option-compatibility gap to be closed for
tidiness. It is the difference between the tool working and not working against
two of the three largest mail providers.

`internal/compat` records the honest position. Of imapsync's 221 options, 31
translate to a native flag, 16 fold into the endpoint URLs, 70 are accepted and
do nothing, and 101 are refused. Nearly every refusal is deliberate and names a
native alternative — `--delete1` refuses because this tool opens the source
read-only, `--regextrans2` refuses and points at `--map`. Only four refuse for
want of implementation, and all four are these:

    oauthdirect1=s        OAuth is not implemented
    oauthdirect2=s        OAuth is not implemented
    oauthaccesstoken1=s   OAuth is not implemented
    oauthaccesstoken2=s   OAuth is not implemented

## 2. The shape of the change

`DialOptions.Password` is a `string`. It is resolved once, at
`cmd/imapsync-go/sync.go:784`, and the same bytes are handed to every dial for
the length of the run.

That is correct for a password and wrong for a token. An access token lives
about an hour; a migration in this tool's intended range runs for several, and
the pool dials new connections throughout — on growth, on reconnection after an
idle close, and on every retry. A token resolved at startup is stale long
before the run ends, and the failure would arrive as authentication errors
scattered across the second half of a migration.

So the credential stops being a value and becomes something that can be asked
again:

```go
// Credential answers with the secret to authenticate with, and says how.
type Credential interface {
	// Secret returns what to authenticate with now.
	Secret(ctx context.Context) (string, error)

	// Refresh is called when the server refused stale. It answers with
	// something better, or reports that there is nothing better to be had.
	Refresh(ctx context.Context, stale string) (string, bool, error)

	Mechanism() Mechanism // LOGIN or XOAUTH2
}
```

### 2.1 Why Refresh is given the value that failed

Passing the *refused* secret back is what makes the rest of the design fall
out, and it does two jobs at once.

The first is correctness. A static password implements `Refresh` as `return "",
false, nil`: a password the server rejected is not going to be accepted on the
second ask, so the original error surfaces unchanged and nothing about existing
runs changes.

The second is that it **is** the single-flight, with no separate machinery.
Forty workers meeting expiry at the same moment all call `Refresh(T)`. The
first finds `stale == current`, runs the command once, and stores `T'`. The
other thirty-nine now find `stale != current`, conclude that somebody has
already replaced what they were holding, and take `T'` without minting
anything. One invocation, no lock convoy, and no separate `singleflight`
dependency to reason about. A mutex around the compare-and-swap is the whole
implementation.

### 2.2 One retry, only on reuse

`authenticate` retries at most once. That bound is what keeps a wrong
credential from becoming an infinite loop of minting and refusal, and it puts
the rule in the same place, and for the same reason, as the SELECT retry in
`Pool.ready`:

> A freshly dialled connection has proven the server is there, so a SELECT it
> refuses is the server refusing, and asking again asks the same question. A
> reused one has proven nothing recent.

Read for credentials it is nearly the same sentence. A token that was just
minted and is still refused means the credentials are wrong — stop, and say so
in the provider's own words. A cached token that is refused has proven nothing
recent, and asking again asks a different question.

This is also why no error taxonomy is needed. There is no table of
`NO [AUTHENTICATIONFAILED]` spellings to keep up to date across providers,
because the decision to retry never depends on *why* the server refused, only
on whether we were holding something that might be out of date.

## 3. The mechanism

Both targets speak XOAUTH2, so one mechanism covers both. `go-sasl` ships
OAUTHBEARER (RFC 7628) but not XOAUTH2, so it is written here: about
twenty-five lines implementing `sasl.Client`.

- `Start()` returns `("XOAUTH2", []byte("user=" + user + "\x01auth=Bearer " +
  token + "\x01\x01"))`.
- `Next(challenge)` parses the provider's JSON error and returns it, which
  makes go-imap cancel the exchange and surface the tagged `NO`.

The second point is the awkward part of XOAUTH2: a failure arrives as a
*challenge* rather than as a refusal, and a client that does not answer it can
desynchronise the connection. `go-sasl`'s own `oauthBearerClient.Next` does
exactly this — unmarshal, return the error — so this follows the library's
handling rather than inventing one. The payoff is that the provider's reason
(`{"status":"401","schemes":"Bearer","scope":...}`) reaches the error text
instead of a bare "authentication failed".

**Tracing needs no change.** `internal/imapx/trace.go` already keeps `tag
AUTHENTICATE <mechanism>` and redacts an inline initial response, as well as
suppressing the client's answer to a continuation. Both paths were covered
before there was a mechanism that used them.

## 4. The minting command

Run under `exec.CommandContext`. The token is stdout, whitespace-trimmed. A
non-zero exit produces an error carrying **stderr only** — never stdout, which
is the secret.

It takes an explicit timeout, and that is not a formality. Commit `584f87e`
found that `Secret.Resolve` could block indefinitely because `security(1)` may
put a keychain prompt in front of the user with nothing able to interrupt it.
`az login` does the same thing. A credential source that can hang is the one
thing that must not hang here, and the difference from the keychain case is
what makes it worse: the keychain is consulted once, at startup, before
anything is waiting. This one is consulted *mid-run*, with a pool blocked
behind it.

## 5. Configuration

```yaml
source:
  url: "imaps://user@outlook.office365.com:993"
  oauth:
    command: "az account get-access-token --resource https://outlook.office365.com --query accessToken -o tsv"
    timeout: 30s        # optional
```

A dedicated `oauth:` block rather than a fourth source on `Secret` and a
separate mechanism switch. The two are genuinely orthogonal — a password could
come from a command too — but this is a file people write by hand, and one
block that reads like what it is beats two knobs that have to be set together.

The block accepts `command:` **or** `file:`, exactly one. A file earns its place
by being genuinely re-consultable: an external agent rewrites it and `Refresh`
re-reads it, which fits the interface with no special casing. It also makes two
imapsync options translate rather than refuse:

| imapsync | becomes |
|---|---|
| `--oauthrefreshcmd1/2` | `oauth: {command: ...}` |
| `--oauthaccesstoken1/2` | `oauth: {file: ...}` |

Flags: `--source-oauth-cmd`, `--dest-oauth-cmd`, `--source-oauth-file`,
`--dest-oauth-file`. Precedence is flag, then config, and the code asks whether
the flag was **given** rather than what it holds — findings 4 and 5 in the main
plan are what that rule cost to learn.

`oauth:` together with `password:` is refused at validation. Guessing which one
wins is how `syncEndpoints` silently dropped `delete2` for the life of the tool.

`probe` gets the same support. Confirming credentials before committing to a
multi-hour migration is most of what `probe` is for.

## 6. What stays refused

`--oauthdirect1/2` — imapsync's "token as a literal string" form — stays
refused, but the reason changes from an admission to a position. A token in
`argv` is readable by every process on the machine through `ps`, and it expires
within the hour, which is the precise failure this design exists to prevent.
The refusal will name `--source-oauth-cmd` and `--source-oauth-file`. That is
the same stance `--delete1` already takes: not "we did not build it" but "we
will not do that".

`--gmail1/2` stays refused for the reason it already gives — it is a bundle of
a byte-rate limit and folder-name rewriting, neither of which exists here — and
gaining OAuth does not change that. `--office1/2` already works, folding into
the endpoint URL.

`--authmech1/2` stays refused. With XOAUTH2 implemented the mechanism is
implied by the credential block, and a flag that could contradict it is a way
to be wrong rather than a capability.

## 7. Testing

The in-memory server does not speak XOAUTH2. Rather than script a server —
which would only ever test the script — put a **TCP proxy in front of the
honest one**, adding `AUTH=XOAUTH2` to CAPABILITY and answering the
AUTHENTICATE exchange itself. That is the harness that caught iCloud's phantom
SEARCH UIDs, and it buys the one behaviour no real provider will perform on
cue: accept a token, then begin refusing it, mid-run.

- the payload decodes to exactly `user=U\x01auth=Bearer T\x01\x01`
- N dials cause **one** command invocation
- many workers refused together cause **one** re-mint, under `-race`
- a *freshly minted* token that is refused fails with the provider's reason,
  having run the command exactly twice
- a token that goes stale mid-run costs no folder
- a static password never calls `Refresh`, and existing behaviour is unchanged
- a hanging command is cut off rather than stalling the pool
- the token never reaches the debug trace

Mutation testing throughout: a mutation that did not apply, or did not compile,
is not a survival.

### 7.1 Against reality

The harness cannot establish that Microsoft and Google accept what we send. Three
things need a real account, and the third is the one that matters:

1. that each provider accepts the framing;
2. which capability each advertises;
3. **what each returns when a token has gone stale.**

The re-mint path fires on a real refusal. The harness only proves it fires on a
refusal we wrote ourselves. That gap — between a server we imagined and a server
that exists — is where every bug this project has found has lived.

## 8. Milestones

- **O0** — the `Credential` interface, with static passwords as the only
  implementation. No behaviour change at all. It is the riskiest edit in the
  design, because it touches the path every existing run takes, so it happens
  alone and is judged by the existing suite passing unchanged.
- **O1** — XOAUTH2, the command and file sources, the cache and its
  compare-and-swap, and the proxy harness.
- **O2** — compat translations, `probe`, configuration, documentation.
- **O3** — verification against real Gmail and Office 365 accounts.

## 9. Decisions and what they cost

1. **A command, not a built-in OAuth2 client.** Every provider-specific
   detail — client ID, secret, tenant, refresh token, scope — lives outside the
   tool and outside its config. No OAuth library, no client secret at rest, no
   callback listener. The cost is that a user without existing tooling has to
   assemble `az` or `gcloud` themselves, and the tool cannot help them past a
   documented recipe.
2. **Discover expiry by being refused, rather than by a TTL.** A TTL means
   trusting a claimed lifetime. This project has had to unlearn that assumption
   twice already: the connection ceiling moved between probes, and the state
   database cannot be trusted over the filesystem. The cost is one failed login
   per expiry, per pool.
3. **`Refresh` takes the refused value.** Buys single-flight for free. The cost
   is an interface that reads oddly until the reason is explained, which is
   what §2.1 is for.
4. **XOAUTH2 only, not OAUTHBEARER.** Both targets advertise XOAUTH2;
   OAUTHBEARER would be a second mechanism serving nobody we have. Revisit when
   a server appears that requires it.
