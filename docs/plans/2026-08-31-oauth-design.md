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
  carries the provider's reason (`{"status":"401","schemes":"Bearer",...}`)
  into the error text instead of a bare "authentication failed".

The second point is the awkward part of XOAUTH2: a failure arrives as a
*challenge* rather than as a refusal. `go-sasl`'s own `oauthBearerClient.Next`
unmarshals and returns the error, and this follows it.

**Correction, found while implementing.** This section first said that
returning an error from `Next` "makes go-imap cancel the exchange and surface
the tagged `NO`". Reading `imapclient/authenticate.go` disproves it: on an
error from `Next` the client returns immediately, without sending the `*` that
would cancel and without reading the tagged response. The connection is left
mid-command, with the server still waiting for a SASL response.

That mattered, because §2.2's retry offered the second secret on the *same*
connection. For LOGIN that is legal; for XOAUTH2 it would have sent the next
command where the server expects a continuation response — a desynchronised
session, and the kind of failure that appears only against a real provider.

**The retry now dials again rather than reusing the connection.** It costs one
handshake on the expiry path, which happens about once an hour per pool, and it
buys two things beyond correcting the bug. It is mechanism-independent, so
nothing about the retry depends on how the secret was presented. And it settles
the question O0 left open and marked for O3 — whether a second `LOGIN` on one
connection is universally safe, given that some servers disconnect after a
failed attempt — by never asking it.

A test asserts one connection per attempt, so the property is not left to the
comment that explains it.

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

### 7.2 What reality said

Run against live Exchange Online (`outlook.office365.com`), and against iCloud
and mox for comparison.

**Question 1, the framing: answered, and it is right.** Exchange advertises
`AUTH=XOAUTH2` and `SASL-IR`, parsed our inline initial response, and rejected
it on the merits rather than on syntax. iCloud advertises `AUTH=XOAUTH2` too;
mox does not, offering only SCRAM, CRAM-MD5 and PLAIN.

**Question 3, the stale token: answered for Microsoft, and not as assumed.**
Exchange sends **no challenge at all**. It goes straight to the tagged
`NO AUTHENTICATE failed.` — there is no base64 JSON status document to read.
Verified twice: through our client, and by hand over a raw TLS socket to rule
out our own parsing.

iCloud does the same, answering a junk token with
`NO [AUTHENTICATIONFAILED] Invalid credentials` and no challenge either. Two of
the two real XOAUTH2 servers reachable from here never send one.

That vindicates §2's decision to treat a refusal **structurally** rather than by
reading the server's text. A design that classified errors by parsing the
challenge would have had nothing whatever to parse against either provider —
including the one this feature exists for.

The re-mint was then confirmed end to end against that real refusal: the token
command ran **twice**, and the second dial was declined by the `fresh == stale`
guard because the minting command returned its cached token. One clear error,
no loop — the behaviour §7 specifies, now observed against a refusal we did not
write.

**Still unverified: a successful XOAUTH2 authentication.** Reaching one needs a
token with `IMAP.AccessAsUser.All`, which no cloud CLI will mint (below), so it
waits for an account with an app registration behind it.

#### The recipes in the first draft of the documentation were wrong

Worth recording because it is the same mistake this project keeps catching:
something written from memory, that reality then refused.

`az account get-access-token --resource https://outlook.office365.com` returns a
token with the right audience and the right account whose scope is
`user_impersonation`, not `IMAP.AccessAsUser.All`. Exchange refuses it. The
Azure CLI's first-party application simply does not hold the IMAP permission,
and neither does the gcloud CLI hold `https://mail.google.com/`. Both providers
require an app registration of your own. The README now says so, having been
corrected by a live server rather than by review.

#### A trace that hid the only line explaining the failure

Found while diagnosing the above, and fixed. The redactor armed its SASL
suppression when it saw `AUTHENTICATE`, on the assumption that the client's
payload came next. With `SASL-IR` the payload is already inline, so nothing
follows — and the next line is the server's verdict, which was duly redacted as
`<redacted SASL payload>`.

So a trace of a failed authentication showed everything except the failure.
Invisible against mox and iCloud, which challenge; guaranteed against Exchange,
which does not.

The suppression is now armed by the server's continuation instead, which is the
only thing a client SASL payload ever follows — in both framings, with and
without `SASL-IR`. Two tests hold the halves apart: the refusal is shown, and a
challenged payload is still hidden. Confirmed against iCloud, which now traces
its refusal in full with the token still redacted.

Mutation testing then cut a guard the restructure had made redundant. Excluding
a continuation from the *consuming* branch survived every test, and thinking
about why showed it was on the wrong side of the redactor's own bias: after the
move, that guard no longer protects a credential, and in the concatenated-line
case the paranoia exists for it would print a line containing the payload.
Hiding one server line beats showing one client secret.

## 8. Milestones

- **O0** *(done, `ab3afb9`)* — the `Credential` interface, with static passwords as the only
  implementation. No behaviour change at all. It is the riskiest edit in the
  design, because it touches the path every existing run takes, so it happens
  alone and is judged by the existing suite passing unchanged.
- **O1** *(done)* — XOAUTH2, the command and file sources, the cache and its
  compare-and-swap, and the proxy harness. Nothing reaches a user yet: no
  configuration names a token source until O2.
- **O2** *(done, `739fd26`)* — compat translations, `probe`, configuration,
  documentation.
- **O3** *(partly done; see §7.2)* — verification against real providers.
- **O4** *(built; awaiting one live Gmail consent)* — `oauth login` and the
  refresh exchange. See §10.

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

## 10. Consent, and the refresh exchange

§9 said getting a token was the provider's business. That was right about
scope and wrong about the user: what it left behind was a shell script every
user has to write, wrapping an exchange that is the same four fields every
time. §7.2 then showed the documentation could not even describe the easy path
correctly. So the tool grows the smallest thing that removes the script.

### 10.1 What rclone can do that we cannot

rclone's OAuth is pleasant because it ships **its own registered client IDs**,
so a user consents and is done. That does not survive contact with mail scopes:

- **Gmail** — `https://mail.google.com/` is *restricted*, not merely sensitive.
  Shipping a shared client ID means verification plus an annual CASA
  assessment. rclone never met this; it does Drive and Photos, not mail.
- **Microsoft** — delegated `IMAP.AccessAsUser.All` *always* requires admin
  consent, so a multi-tenant client ID we ship still needs every tenant's admin
  to act. Shipping one buys nothing for the accounts that need it.

So the user brings a client ID either way. The mechanics transfer; the shortcut
does not. Registering an app is five minutes once. Writing a correct refresh
exchange is not, and that is the part worth deleting.

### 10.2 One blob, carried by the existing Secret

A refresh exchange needs a client ID, an optional client secret, a refresh
token and a token endpoint. Rather than four config fields and twelve flags,
they travel as one JSON document held in a `config.Secret`:

```yaml
oauth:
  refresh:
    keychain: gmail-jens          # or file:, or env:
```

```json
{
  "client_id": "....apps.googleusercontent.com",
  "client_secret": "...",
  "refresh_token": "...",
  "token_uri": "https://oauth2.googleapis.com/token"
}
```

`Secret` already resolves env, file and the macOS keychain, so this needs no
token store and no new storage code: the keychain on macOS and a file elsewhere
is a choice the user makes per endpoint rather than one the tool makes for
them. Nothing is named for a provider, so two Gmail accounts are two secrets
with two names — the naming is the user's.

The blob is also the portable format. Moving a credential to a migration box is
copying one file, which is the headless case without a second consent flow to
maintain. It is deliberately the shape of Google's own `authorized_user` file.

`token_uri` is required rather than defaulted. It is where a refresh token gets
POSTed, and a guessed host is not a risk worth taking to save a line.

### 10.3 Rotation, and what we do not do

Providers may return a new refresh token on each exchange. We cannot write back
into an environment variable, and rewriting a keychain item behind the user's
back is worse than the problem. So the newest refresh token is held **in memory
for the life of the process**, which is exactly the case that matters: a
migration is one long process. A run that ends and is started again reads the
stored one, which is still valid for both providers in normal use.

When the exchange itself fails with `invalid_grant` the refresh token is dead,
and the only useful thing to say is that the consent must be redone. There is
no retry that helps.

**Google's publishing status decides how long that takes to happen.** An app
left in *Testing* issues refresh tokens that expire after **seven days**; *In
production* issues long-lived ones, and is a separate axis from verification —
unverified merely means a warning screen and a hundred-user cap.

### 10.4 The consent

A subcommand rather than a documented script, because a script is another thing
to be wrong in the documentation and does not travel:

```
imapsync-go oauth login --client-file client_secret_….json --out secrets/gmail.json
```

Loopback, per RFC 8252: bind `127.0.0.1:0`, take whatever port the kernel gives,
open a browser, catch the redirect, exchange the code. **PKCE (S256) is not
optional** — Microsoft requires it of public clients, and Google recommends it.
A `state` value is checked for the same reason.

`--client-file` reads Google's installed-app JSON directly, so the client ID,
secret and both endpoints come from the file the console downloads and the user
pastes nothing. Explicit `--client-id`/`--auth-url`/`--token-url` cover
Microsoft, where the console hands over no such file.

Output goes to `--out`, `--keychain` or `--stdout`, exactly one, chosen rather
than defaulted: the thing being written is a long-lived credential and it should
not reach a terminal by accident.

What this is **not**: a token store, an account registry, an export/import pair,
or a device-code flow. The blob is the format, the `Secret` is the store, and a
laptop consent plus a file copy is the headless story.

### 10.5 What the build found that the design did not anticipate

Two things, both discovered by running the code against the real thing rather
than by reading a manual.

**The keychain cannot be written the obvious way, or the second-obvious way.**
`security add-generic-password -w VALUE` puts the credential in the argument
vector, where `ps` can read it -- the same objection this design already makes
to `--oauthdirect`, and worse here, because this credential is good for months
rather than an hour. The documented alternative, `-w` with no value, prompts
for the password instead; it reads that prompt the way `getpass` does and
**silently truncates at 128 bytes**. A real credential is around 350. It would
have stored a corrupt document and failed on the first sync, days later, with
an error naming the wrong thing.

What works is `security -i`, where the whole command line arrives on standard
input, with the value hex-encoded via `-X`. Hex because the interactive parser
splits on whitespace and honours quotes, and a client secret is free to contain
both. Interactive mode also reports a failed sub-command in its output while
still exiting zero, so the exit status alone cannot be believed.

This is exactly the class of thing a unit test cannot find, so the test that
covers it is a **round trip through the real `security(1)` on both sides**:
`oauth login --keychain` writes, `config.Secret` reads, and the credential used
contains a space, a double quote and a backslash. The two halves live in
different packages and neither one alone can show they agree.

**Suppressing a lint false positive revealed a true finding underneath.** The
same pattern as the `Secret.Resolve` context fix in the previous phase:
golangci-lint reports one issue per line, so annotating the `gosec` finding on
the browser-opening lines exposed a `noctx` finding on the same lines that had
been hidden by it. `OpenBrowser` now takes a context.

The context bounds the *opener*, not the browser -- all three openers hand the
URL to a running desktop and exit, so by the time a consent finishes, minutes
later, the process being cancelled is long gone. What it protects against is
the opposite case: an opener that hangs because there is no desktop to hand to.
The child is also now reaped, since a process that may run for hours should not
accumulate zombies.
