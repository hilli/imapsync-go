package imapx

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hilli/imapsync-go/internal/config"
)

// scriptedCredential offers one secret, then whatever the script says when the
// server refuses it. It records what it was asked, because the number of LOGIN
// attempts a refusal provokes is the whole of what this milestone changed.
type scriptedCredential struct {
	secret string

	// better is what Refresh offers. Empty means it declines, which is what a
	// static password always does.
	better string

	// secretErr, when set, makes Secret fail rather than answer.
	secretErr error

	// refreshErr, when set, makes Refresh fail rather than decline.
	refreshErr error

	secrets   atomic.Int32
	refreshes atomic.Int32
	sawStale  atomic.Value
}

func (c *scriptedCredential) Secret(context.Context) (string, error) {
	c.secrets.Add(1)
	if c.secretErr != nil {
		return "", c.secretErr
	}
	return c.secret, nil
}

func (c *scriptedCredential) Refresh(_ context.Context, stale string) (string, bool, error) {
	c.refreshes.Add(1)
	c.sawStale.Store(stale)
	if c.refreshErr != nil {
		return "", false, c.refreshErr
	}
	if c.better == "" {
		return "", false, nil
	}
	return c.better, true, nil
}

func dialCredential(t *testing.T, srv *fakeServer, cred Credential) (Conn, error) {
	t.Helper()

	host, port := splitHostPortForTest(t, srv.addr())
	return Dial(context.Background(), DialOptions{
		Addr: config.Address{
			Host: host,
			Port: port,
			User: "user",
			TLS:  config.TLSNone,
		},
		Credential: cred,
	})
}

func logins(t *testing.T, srv *fakeServer) int {
	t.Helper()
	return len(srv.commandsMatching("LOGIN"))
}

// TestAPasswordTheServerRefusedIsNotOfferedTwice. A static password cannot
// become right by being sent again, so a wrong one must stay a single clear
// failure rather than doubling every login attempt in the run.
func TestAPasswordTheServerRefusedIsNotOfferedTwice(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})

	conn, err := dialCredential(t, srv, StaticPassword("wrong"))
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial() succeeded with a password the server refuses")
	}
	if got := logins(t, srv); got != 1 {
		t.Errorf("server saw %d LOGIN commands, want 1", got)
	}
}

// TestACredentialWithSomethingBetterIsRetriedOnce is the path a token takes
// when it expired mid-run: the server refuses, the credential mints a fresh
// one, and the dial succeeds without the caller ever seeing a failure.
func TestACredentialWithSomethingBetterIsRetriedOnce(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})
	cred := &scriptedCredential{secret: "stale", better: "right"}

	conn, err := dialCredential(t, srv, cred)
	if err != nil {
		t.Fatalf("Dial() error = %v, want success after refresh", err)
	}
	defer func() { _ = conn.Close() }()

	if got := logins(t, srv); got != 2 {
		t.Errorf("server saw %d LOGIN commands, want 2", got)
	}
	if got := cred.refreshes.Load(); got != 1 {
		t.Errorf("Refresh called %d times, want 1", got)
	}
}

// TestRefreshIsToldWhichSecretWasRefused pins the contract the caching
// credential will rest on: knowing which value failed is what lets many
// workers offering the same dead token provoke a single mint.
func TestRefreshIsToldWhichSecretWasRefused(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})
	cred := &scriptedCredential{secret: "stale", better: "right"}

	conn, err := dialCredential(t, srv, cred)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	stale, _ := cred.sawStale.Load().(string)
	if stale != "stale" {
		t.Errorf("Refresh was told the refused secret was %q, want %q", stale, "stale")
	}
}

// TestARenewalTheServerAlsoRefusesStops. The retry is bounded at one, so a
// credential that keeps producing rejected secrets fails the dial instead of
// hammering the server -- which is how an account gets locked out.
func TestARenewalTheServerAlsoRefusesStops(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})
	cred := &scriptedCredential{secret: "stale", better: "also-wrong"}

	conn, err := dialCredential(t, srv, cred)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial() succeeded with two refused secrets")
	}
	if got := logins(t, srv); got != 2 {
		t.Errorf("server saw %d LOGIN commands, want 2", got)
	}
}

// TestACredentialThatCannotAnswerNeverReachesTheServer. A token command that
// fails is a local problem, and sending an empty password in its place would
// report it as an authentication failure against the account.
func TestACredentialThatCannotAnswerNeverReachesTheServer(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})
	boom := errors.New("token command exited 1")
	cred := &scriptedCredential{secretErr: boom}

	conn, err := dialCredential(t, srv, cred)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial() succeeded without a secret")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Dial() error = %v, want it to carry %v", err, boom)
	}
	if got := logins(t, srv); got != 0 {
		t.Errorf("server saw %d LOGIN commands, want 0", got)
	}
}

// TestARenewalThatFailedNamesBothCauses. When a token command breaks while the
// server is refusing, either cause alone misleads: the refusal without the
// command's error looks like a wrong password, and the command's error without
// the refusal looks like it broke unprompted.
func TestARenewalThatFailedNamesBothCauses(t *testing.T) {
	srv := startFakeServer(t, fakeServerOptions{caps: "IMAP4rev1", acceptPassword: "right"})
	boom := errors.New("token command exited 1")
	cred := &scriptedCredential{secret: "stale", refreshErr: boom}

	conn, err := dialCredential(t, srv, cred)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial() succeeded though renewal failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Dial() error = %v, want it to carry the renewal failure %v", err, boom)
	}
	if !strings.Contains(err.Error(), "AUTHENTICATIONFAILED") {
		t.Errorf("Dial() error = %v, want it to carry the server's own refusal", err)
	}
	if got := logins(t, srv); got != 1 {
		t.Errorf("server saw %d LOGIN commands, want 1", got)
	}
}

// TestAStaticPasswordHasNothingBetter states the property that keeps a typo in
// a config file behaving as it always has, now that the dial path is willing
// to ask twice.
func TestAStaticPasswordHasNothingBetter(t *testing.T) {
	secret, better, err := StaticPassword("hunter2").Refresh(context.Background(), "hunter2")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if better {
		t.Errorf("Refresh() offered %q, want a static password to decline", secret)
	}
}
