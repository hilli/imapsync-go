package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/oauthx"
)

// A login has to say where the credential goes. It is the one flag with no
// safe default: a credential good for months should not reach a scrollback
// buffer because nobody chose.
func TestLoginRefusesToGuessWhereTheCredentialGoes(t *testing.T) {
	t.Parallel()

	f := oauthLoginFlags{
		clientID: "id", authURL: "https://example.test/auth", tokenURL: "https://example.test/token",
		scope: []string{"https://mail.google.com/"},
	}

	err := runOAuthLogin(context.Background(), io.Discard, f)
	if err == nil {
		t.Fatal("a login with no destination was accepted")
	}
	for _, want := range []string{"--out", "--keychain", "--stdout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %v", want, err)
		}
	}
}

// Without a scope the consent grants nothing, and the error should say which
// scope the caller probably wanted rather than merely that one is missing.
func TestLoginRefusesAConsentThatWouldGrantNothing(t *testing.T) {
	t.Parallel()

	f := oauthLoginFlags{
		clientID: "id", authURL: "https://example.test/auth", tokenURL: "https://example.test/token",
		stdout: true,
	}

	err := runOAuthLogin(context.Background(), io.Discard, f)
	if err == nil {
		t.Fatal("a login with no scope was accepted")
	}
	if !strings.Contains(err.Error(), "mail.google.com") {
		t.Errorf("the refusal does not name a scope to try: %v", err)
	}
}

// The Google console's file is the expected way in, so its shape must be
// understood without the user restating any of it.
func TestClientFileIsReadWholesale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "client_secret.json")
	writeTestFile(t, path, `{"installed":{
		"client_id":"1234.apps.googleusercontent.com",
		"client_secret":"shh",
		"auth_uri":"https://accounts.google.com/o/oauth2/auth",
		"token_uri":"https://oauth2.googleapis.com/token",
		"redirect_uris":["http://localhost"]}}`)

	client, err := loginClient(oauthLoginFlags{clientFile: path})
	if err != nil {
		t.Fatalf("reading the client file: %v", err)
	}

	if client.ID != "1234.apps.googleusercontent.com" {
		t.Errorf("client id is %q", client.ID)
	}
	if client.Secret != "shh" {
		t.Errorf("client secret is %q", client.Secret)
	}
	if client.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Errorf("token endpoint is %q", client.TokenURL)
	}
}

// A tenant-specific endpoint is the one thing a downloaded file can get wrong,
// so a flag beside a file overrides rather than colliding.
func TestAnEndpointFlagOverridesTheClientFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "client_secret.json")
	writeTestFile(t, path, `{"installed":{"client_id":"id","auth_uri":"https://a.test/","token_uri":"https://b.test/"}}`)

	client, err := loginClient(oauthLoginFlags{
		clientFile: path,
		authURL:    "https://tenant.test/authorize",
		tokenURL:   "https://tenant.test/token",
	})
	if err != nil {
		t.Fatalf("reading the client file: %v", err)
	}
	if client.TokenURL != "https://tenant.test/token" {
		t.Errorf("the token endpoint flag did not win: %q", client.TokenURL)
	}
	if client.AuthURL != "https://tenant.test/authorize" {
		t.Errorf("the authorization endpoint flag did not win: %q", client.AuthURL)
	}
	if client.ID != "id" {
		t.Errorf("a field no flag named changed: %q", client.ID)
	}
}

// A file written 0644 would put a months-long credential where every account
// on the machine can read it.
func TestTheWrittenCredentialIsReadableOnlyByItsOwner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cred.json")
	var out bytes.Buffer

	err := storeCredential(context.Background(), &out, oauthLoginFlags{out: path},
		[]byte("{\n  \"refresh_token\": \"r\"\n}\n"), []byte(`{"refresh_token":"r"}`))
	if err != nil {
		t.Fatalf("storing the credential: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the credential file is mode %o, not 600", perm)
	}
	if !strings.Contains(out.String(), "--source-oauth-refresh-file") {
		t.Errorf("the user is not told how to use what was just written: %q", out.String())
	}
}

// The pair that matters: what `oauth login --keychain` writes must be exactly
// what a sync reads back, through the real security(1) on both sides. The two
// halves live in different packages and neither one alone can show they agree.
func TestAKeychainCredentialRoundTripsThroughTheReader(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain is macOS only")
	}

	const service = "imapsync-go-roundtrip-test"
	ctx := context.Background()
	t.Cleanup(func() { _ = deleteKeychainItem(ctx, service) })

	creds := oauthx.Credentials{
		ClientID:     "1234.apps.googleusercontent.com",
		ClientSecret: `GOCSPX-a secret with spaces and "quotes" and \\`,
		RefreshToken: "1//0gA_token-with_punctuation.and~more",
		TokenURI:     "https://oauth2.googleapis.com/token",
	}
	compact, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	if err := keychainStore(ctx, service, compact); err != nil {
		t.Fatalf("storing: %v", err)
	}

	blob, err := config.Secret{Keychain: service}.Resolve(ctx)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	got, err := oauthx.ParseCredentials(blob)
	if err != nil {
		t.Fatalf("parsing what came back: %v", err)
	}
	if got != creds {
		t.Errorf("what came back is not what went in:\n got %+v\nwant %+v", got, creds)
	}
}

// deleteKeychainItem removes the item a test left behind.
func deleteKeychainItem(ctx context.Context, service string) error {
	return exec.CommandContext(ctx, "security", "delete-generic-password", "-s", service).Run() //nolint:gosec // a constant in a test
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
