package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
)

// TestAnOAuthFlagReplacesTheConfiguredSourceOutright. Merging field by field
// would let a --source-oauth-file land beside a configured command and produce
// a block naming both, which validation then refuses -- an error about a
// combination the user never wrote. Replacing outright means the flag says
// where the token comes from, entirely.
func TestAnOAuthFlagReplacesTheConfiguredSourceOutright(t *testing.T) {
	t.Parallel()

	configured := config.OAuth{Command: "az account get-access-token", Timeout: 20 * time.Second}

	// A flag naming the other source displaces the command and the timeout
	// that bounded it.
	got := oauthFrom(configured, "", "/tmp/token")
	if got.Command != "" {
		t.Errorf("oauth after --oauth-file = %+v; the configured command survived", got)
	}
	if got.File != "/tmp/token" {
		t.Errorf("oauth after --oauth-file = %+v; the flag's file is missing", got)
	}
	if got.Timeout != 0 {
		t.Errorf("oauth after --oauth-file = %+v; a timeout that bounded the displaced command survived", got)
	}

	// A flag naming the same source still replaces it, so the flag's value
	// wins rather than the config's.
	got = oauthFrom(configured, "gcloud auth print-access-token", "")
	if got.Command != "gcloud auth print-access-token" {
		t.Errorf("oauth after --oauth-cmd = %+v; the config's command won", got)
	}

	// Neither flag given leaves the configured block exactly as written,
	// timeout included.
	got = oauthFrom(configured, "", "")
	if got != configured {
		t.Errorf("oauth with no flag = %+v; want the configured block %+v unchanged", got, configured)
	}
}

// TestATokenFlagBesideAConfiguredPasswordIsRefused. The merge happens before
// validation for exactly this: a config that authenticates with a password and
// a flag that authenticates with a token disagree about which credential the
// server sees, and the run must not pick one quietly.
func TestATokenFlagBesideAConfiguredPasswordIsRefused(t *testing.T) {
	t.Parallel()

	ep := config.Endpoint{
		URL:      "imaps://user@example.test",
		Password: config.Secret{Env: "SOME_PASSWORD"},
	}
	ep.OAuth = oauthFrom(ep.OAuth, "get-token", "")

	if err := ep.Validate(); err == nil {
		t.Fatal("a token flag beside a configured password was accepted")
	}
}

// TestTheCredentialFollowsFromTheEndpoint checks the three shapes end to end,
// by what each one authenticates with rather than by its Go type: a token
// source is consulted when the secret is asked for, and a password is not.
func TestTheCredentialFollowsFromTheEndpoint(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	t.Run("a file source reads the file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(dir, "token")
		if err := os.WriteFile(path, []byte("from-the-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		ep := config.Endpoint{URL: "imaps://user@example.test", OAuth: config.OAuth{File: path}}
		cred, err := imapx.CredentialFor(ctx, ep)
		if err != nil {
			t.Fatalf("CredentialFor: %v", err)
		}
		if cred.Mechanism() != imapx.MechanismXOAUTH2 {
			t.Errorf("mechanism = %v; a token authenticates with XOAUTH2", cred.Mechanism())
		}
		secret, err := cred.Secret(ctx)
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		if secret != "from-the-file" {
			t.Errorf("secret = %q; want the file's contents, trimmed", secret)
		}
	})

	t.Run("a command source runs the command", func(t *testing.T) {
		t.Parallel()

		ep := config.Endpoint{
			URL:   "imaps://user@example.test",
			OAuth: config.OAuth{Command: "printf from-the-command"},
		}
		cred, err := imapx.CredentialFor(ctx, ep)
		if err != nil {
			t.Fatalf("CredentialFor: %v", err)
		}
		secret, err := cred.Secret(ctx)
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		if secret != "from-the-command" {
			t.Errorf("secret = %q; want what the command printed", secret)
		}
	})

	t.Run("a password authenticates with LOGIN", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(dir, "password")
		if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		ep := config.Endpoint{
			URL:      "imaps://user@example.test",
			Password: config.Secret{File: path},
		}
		cred, err := imapx.CredentialFor(ctx, ep)
		if err != nil {
			t.Fatalf("CredentialFor: %v", err)
		}
		if cred.Mechanism() != imapx.MechanismLogin {
			t.Errorf("mechanism = %v; a password authenticates with LOGIN", cred.Mechanism())
		}
		secret, err := cred.Secret(ctx)
		if err != nil {
			t.Fatalf("Secret: %v", err)
		}
		if secret != "hunter2" {
			t.Errorf("secret = %q; want the resolved password", secret)
		}
	})
}

// TestATokenIsMintedOncePerEndpointRatherThanPerConnection. The credential is
// built once and handed to every dial, which is what makes an expiry a single
// re-mint rather than one per connection. Building it inside the dial function
// would still work and would run the command once per connection -- forty
// times on the source side of a real pair.
func TestATokenIsMintedOncePerEndpointRatherThanPerConnection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "mint.sh")
	body := "#!/bin/sh\necho x >> " + counter + "\nprintf a-token\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // a test fixture that must be executable
		t.Fatal(err)
	}

	ep := config.Endpoint{URL: "imaps://user@example.test", OAuth: config.OAuth{Command: script}}
	cred, err := imapx.CredentialFor(context.Background(), ep)
	if err != nil {
		t.Fatalf("CredentialFor: %v", err)
	}

	for range 5 {
		if _, err := cred.Secret(context.Background()); err != nil {
			t.Fatalf("Secret: %v", err)
		}
	}

	runs := 0
	if b, err := os.ReadFile(counter); err == nil {
		runs = strings.Count(string(b), "x")
	}
	if runs != 1 {
		t.Errorf("the minting command ran %d times for 5 dials; want 1", runs)
	}
}
