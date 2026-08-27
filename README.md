# imapsync-go

A Go implementation of the ideas behind Perl [imapsync]: one-way migration and
repeated synchronisation of IMAP accounts.

The reason to rewrite it is **concurrency**. IMAP is strictly request/response
per connection, so throughput comes from running many connections and slicing
work across them — folder-parallel *and* intra-folder parallel, so a single
200k-message INBOX still saturates the available connections.

> **Status:** early development. Milestone M1 works: `probe` reports what a
> server supports, and `sync` performs a correct, resumable, one-way copy over a
> single connection per side. The concurrency the project exists for is M2 and
> does not exist yet. See [the design document](docs/plans/2026-08-27-imapsync-go-design.md).

## Build and run

```sh
go build -o imapsync-go ./cmd/imapsync-go
```

To run without building, pass the **package**, not the file:

```sh
go run ./cmd/imapsync-go probe --help     # correct
go run cmd/imapsync-go/main.go probe      # fails: compiles only main.go
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

`--max-connections` opens connections until the server refuses. It is off by
default because it is intrusive; iCloud in particular throttles aggressively.

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

Inline passwords (`imaps://user:pass@host`) are rejected. Credentials come from
`--password-env`, `--password-file` or `--password-keychain` (macOS).

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

2 folders, 3 copied, 0 adopted, 0 failed, in 5ms
```

Use `--dry-run` first: it reports the folders it would create and the messages
it would copy, and writes nothing to either the destination or the state
database.

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

### Self-signed servers

TLS verification is controlled **per side**: `--source-insecure` and
`--dest-insecure`. A self-signed destination on your own network is a
reasonable thing to accept; accepting it must not also stop verifying a public
source reached over the internet, so there is no single flag that does both.

macOS rejects a self-signed certificate whose validity exceeds 398 days with
`certificate is not standards compliant`, and adding it to the keychain does not
help. Either reissue it with a shorter lifetime or pass `--dest-insecure`.

## Configuration

For recurring syncs, use a config file instead of a long flag line. See
[`imapsync.example.yaml`](imapsync.example.yaml). Credentials are always
*referenced*, never stored inline.

```sh
go run ./cmd/imapsync-go probe --config imapsync.yaml --pair icloud-to-mox --side both
```

## Development

```sh
go build ./...
go vet ./...
golangci-lint run ./...
go test -race ./...
```

Tests run against go-imap's in-process `imapmemserver`, so no network or
container is needed.

## Licence

MIT

[imapsync]: https://github.com/imapsync/imapsync
