package imapx_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/imapx"
)

// hostPort splits a listener address for DialOptions.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing port %q: %v", port, err)
	}
	return host, n
}

func dialThrough(t *testing.T, addr string, cred imapx.Credential) (imapx.Conn, error) {
	t.Helper()

	host, port := hostPort(t, addr)
	return imapx.Dial(context.Background(), imapx.DialOptions{
		Addr: config.Address{
			Host: host,
			Port: port,
			User: testUser,
			TLS:  config.TLSNone,
		},
		Credential: cred,
	})
}

// runsOf reports how many times the minting script has run.
func runsOf(t *testing.T, counter string) int {
	t.Helper()

	raw, err := os.ReadFile(counter) //nolint:gosec // a path this test made
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

// traceBuffer collects the debug trace from the goroutine that writes it.
type traceBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *traceBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *traceBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestTheTokenGoesOnTheWireInTheFramingProvidersSpecify. The doorman insists on
// the exact bytes rather than merely finding the token, because a payload that
// is nearly right is refused by Google with the same message as a token that
// is wrong.
func TestTheTokenGoesOnTheWireInTheFramingProvidersSpecify(t *testing.T) {
	for _, saslIR := range []bool{true, false} {
		name := "with SASL-IR"
		if !saslIR {
			name = "without SASL-IR, answering a continuation"
		}
		t.Run(name, func(t *testing.T) {
			upstream, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
			door := startDoorman(t, upstream, testUser, testPassword, "good-token", saslIR)

			conn, err := dialThrough(t, door.addr, imapx.FileToken(writeToken(t, "good-token")))
			if err != nil {
				t.Fatalf("Dial() error = %v", err)
			}
			defer func() { _ = conn.Close() }()

			seen := door.tokensSeen()
			if len(seen) != 1 || seen[0] != "good-token" {
				t.Errorf("server saw tokens %q, want exactly [good-token]", seen)
			}
		})
	}
}

// TestAnExpiredTokenIsReplacedWithoutTheCallerNoticing is the whole point of
// the design: a run lasting hours outlives its token, and the pool dials new
// connections throughout.
func TestAnExpiredTokenIsReplacedWithoutTheCallerNoticing(t *testing.T) {
	upstream, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	door := startDoorman(t, upstream, testUser, testPassword, "first", true)

	path := writeToken(t, "first")
	cred := imapx.FileToken(path)

	conn, err := dialThrough(t, door.addr, cred)
	if err != nil {
		t.Fatalf("first Dial() error = %v", err)
	}
	_ = conn.Close()

	// The token expires and the agent that maintains it writes a new one.
	door.expire("second")
	if werr := os.WriteFile(path, []byte("second"), 0o600); werr != nil {
		t.Fatalf("rewriting token: %v", werr)
	}

	conn, err = dialThrough(t, door.addr, cred)
	if err != nil {
		t.Fatalf("second Dial() error = %v, want the expiry to be absorbed", err)
	}
	defer func() { _ = conn.Close() }()

	// One connection per attempt: the first dial, the refused retry, and the
	// one that carried the renewed token. Offering the second token on the
	// refused connection would be a byte behind wherever the server is: an
	// XOAUTH2 refusal arrives as a challenge, so the exchange is still open.
	if got := door.conns.Load(); got != 3 {
		t.Errorf("the server accepted %d connections, want 3 -- one per attempt", got)
	}

	seen := door.tokensSeen()
	want := []string{"first", "first", "second"}
	if len(seen) != len(want) {
		t.Fatalf("server saw tokens %q, want %q", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("server saw tokens %q, want %q", seen, want)
		}
	}
}

// TestManyWorkersMeetingOneExpiryMintOnce. A pool meets an expiry all at once,
// and a credential that minted per refusal would answer a burst of forty
// refusals with forty commands -- which is how a provider starts rate-limiting.
func TestManyWorkersMeetingOneExpiryMintOnce(t *testing.T) {
	upstream, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	door := startDoorman(t, upstream, testUser, testPassword, "stale", true)

	dir := t.TempDir()
	script, counter := mintingScript(t, dir, "stale", "fresh")
	cred := imapx.CommandToken(script, 10*time.Second)

	// Everyone starts holding the token the door is about to stop accepting.
	if _, err := cred.Secret(context.Background()); err != nil {
		t.Fatalf("priming: %v", err)
	}
	if runsOf(t, counter) != 1 {
		t.Fatalf("priming ran the command %d times, want 1", runsOf(t, counter))
	}
	door.expire("fresh")

	const workers = 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := dialThrough(t, door.addr, cred)
			if err != nil {
				errs <- err
				return
			}
			_ = conn.Close()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a worker failed to dial: %v", err)
	}
	if got := runsOf(t, counter); got != 2 {
		t.Errorf("the command ran %d times, want 2 (one to prime, one to renew)", got)
	}
}

// TestAFreshlyMintedTokenThatIsRefusedStops. Renewing a token the server has
// just refused asks the same question again; without the guard a wrong
// configuration becomes an unbounded loop of minting against the provider.
func TestAFreshlyMintedTokenThatIsRefusedStops(t *testing.T) {
	upstream, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	door := startDoorman(t, upstream, testUser, testPassword, "the-only-good-token", true)

	dir := t.TempDir()
	script, counter := mintingScript(t, dir, "wrong")
	cred := imapx.CommandToken(script, 10*time.Second)

	conn, err := dialThrough(t, door.addr, cred)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial() succeeded with a token the server refuses")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Dial() error = %v, want the provider's own explanation", err)
	}
	if got := runsOf(t, counter); got != 2 {
		t.Errorf("the command ran %d times, want 2", got)
	}
	if got := len(door.tokensSeen()); got != 1 {
		t.Errorf("the server saw %d tokens, want 1 -- the guard should decline before dialling again", got)
	}
}

// TestAHangingTokenCommandIsCutOff. This is the failure the keychain taught:
// a credential source that can block for ever, consulted mid-run with a pool
// waiting behind it.
func TestAHangingTokenCommandIsCutOff(t *testing.T) {
	cred := imapx.CommandToken("sleep 60", 200*time.Millisecond)

	start := time.Now()
	_, err := cred.Secret(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Secret() returned with no error from a command that never finishes")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Secret() took %v to give up, want the timeout to cut it off", elapsed)
	}
}

// TestAFailingTokenCommandReportsStderrOnly. A command that prints a token and
// then fails must not put the token in the logs.
func TestAFailingTokenCommandReportsStderrOnly(t *testing.T) {
	cred := imapx.CommandToken("echo super-secret-token; echo 'not logged in' >&2; exit 1", 10*time.Second)

	_, err := cred.Secret(context.Background())
	if err == nil {
		t.Fatal("Secret() succeeded though the command failed")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("Secret() error = %v, want it to keep the token out of the message", err)
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("Secret() error = %v, want it to carry what the command said", err)
	}
}

// TestTheTokenNeverReachesTheTrace. --debug writes the session to a file, and
// an access token is a bearer credential for the whole mailbox.
func TestTheTokenNeverReachesTheTrace(t *testing.T) {
	upstream, _ := startMemServer(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	door := startDoorman(t, upstream, testUser, testPassword, "sensitive-token", true)

	var trace traceBuffer
	host, port := hostPort(t, door.addr)
	conn, err := imapx.Dial(context.Background(), imapx.DialOptions{
		Addr:        config.Address{Host: host, Port: port, User: testUser, TLS: config.TLSNone},
		Credential:  imapx.FileToken(writeToken(t, "sensitive-token")),
		DebugWriter: &trace,
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	if got := trace.String(); strings.Contains(got, "sensitive-token") {
		t.Errorf("the trace contains the token:\n%s", got)
	}
	if !strings.Contains(trace.String(), "AUTHENTICATE") {
		t.Errorf("the trace does not mention AUTHENTICATE at all:\n%s", trace.String())
	}
}

// TestAnEmptyTokenSourceIsReportedRatherThanOffered. An empty token is refused
// by the provider as a wrong credential, which sends the reader looking in the
// wrong place.
func TestAnEmptyTokenSourceIsReportedRatherThanOffered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	if _, err := imapx.FileToken(path).Secret(context.Background()); err == nil {
		t.Error("Secret() accepted an empty token file")
	}

	if _, err := imapx.CommandToken("true", time.Second).Secret(context.Background()); err == nil {
		t.Error("Secret() accepted a command that printed nothing")
	}
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}
	return path
}

// mintingScript writes a shell script that prints the given tokens in turn --
// the Nth run prints the Nth, and the last one thereafter -- and records each
// run, so a test can count invocations rather than infer them.
func mintingScript(t *testing.T, dir string, tokens ...string) (string, string) {
	t.Helper()

	counter := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "mint.sh")

	var b strings.Builder
	b.WriteString("#!/bin/sh\necho x >> " + counter + "\n")
	b.WriteString("case $(wc -l < " + counter + " | tr -d ' ') in\n")
	for i, tok := range tokens {
		fmt.Fprintf(&b, "  %d) echo %s ;;\n", i+1, tok)
	}
	fmt.Fprintf(&b, "  *) echo %s ;;\nesac\n", tokens[len(tokens)-1])

	if err := os.WriteFile(script, []byte(b.String()), 0o700); err != nil { //nolint:gosec // a script has to be executable
		t.Fatalf("writing script: %v", err)
	}
	return "sh " + script, counter
}
