package probe_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hilli/imapsync-go/internal/config"
	"github.com/hilli/imapsync-go/internal/probe"
)

const (
	testUser     = "probe@example.test"
	testPassword = "hunter2"
)

// startMemServer runs an in-process IMAP server and returns its address.
func startMemServer(t *testing.T) string {
	t.Helper()

	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPassword)
	for _, mailbox := range []string{"INBOX", "Archive", "Sent Messages", "Deleted Messages"} {
		if err := user.Create(mailbox, nil); err != nil {
			t.Fatalf("creating mailbox %q: %v", mailbox, err)
		}
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String()
}

func testEndpoint(t *testing.T, addr string) config.Endpoint {
	t.Helper()
	t.Setenv("IMAPSYNC_PROBE_TEST_PW", testPassword)
	return config.Endpoint{
		URL:      "imap+insecure://" + testUser + "@" + addr,
		Password: config.Secret{Env: "IMAPSYNC_PROBE_TEST_PW"},
	}
}

func TestRunReportsCapabilitiesAndFolders(t *testing.T) {
	addr := startMemServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := probe.Run(ctx, probe.Options{
		Endpoint:   testEndpoint(t, addr),
		WithStatus: true,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if !report.Caps.IMAP4rev2 {
		t.Error("expected IMAP4rev2 to be advertised")
	}
	// IMAP4rev2 subsumes these, so they must be reported even though the server
	// does not advertise them separately.
	if !report.Caps.UIDPlus {
		t.Error("expected UIDPLUS to be implied by IMAP4rev2")
	}
	if !report.Caps.SpecialUse {
		t.Error("expected SPECIAL-USE to be implied by IMAP4rev2")
	}

	names := make(map[string]bool, len(report.Folders))
	for _, f := range report.Folders {
		names[f.Name] = true
	}
	for _, want := range []string{"INBOX", "Archive", "Sent Messages", "Deleted Messages"} {
		if !names[want] {
			t.Errorf("folder %q missing from report", want)
		}
	}

	if report.MaxConnections != 0 {
		t.Errorf("ceiling should not be measured by default, got %d", report.MaxConnections)
	}
	if report.Elapsed <= 0 {
		t.Error("expected a positive elapsed time")
	}
}

func TestRunMeasuresConnectionCeiling(t *testing.T) {
	addr := startMemServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const cap = 4
	report, err := probe.Run(ctx, probe.Options{
		Endpoint:       testEndpoint(t, addr),
		MaxConnections: cap,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	// The in-memory server imposes no limit, so the search should stop at the cap.
	if report.MaxConnections != cap {
		t.Errorf("MaxConnections = %d, want %d", report.MaxConnections, cap)
	}
	if report.CeilingLimitedBy == "" {
		t.Error("expected a reason for the ceiling result")
	}

	// Nothing was refused, so no ceiling was found and there is nothing to
	// stay clear of. Suggesting cap-1 here would be advising against a limit
	// that was never observed, and would quietly recommend one fewer
	// connection every time the probe is run with a larger cap.
	if report.Refused {
		t.Error("the in-memory server refuses nothing; Refused must be false")
	}
	if got, want := report.SuggestedConcurrency(), cap; got != want {
		t.Errorf("SuggestedConcurrency() = %d, want %d", got, want)
	}
}

func TestRunRejectsBadCredentials(t *testing.T) {
	addr := startMemServer(t)
	t.Setenv("IMAPSYNC_PROBE_TEST_BAD_PW", "wrong-password")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := probe.Run(ctx, probe.Options{
		Endpoint: config.Endpoint{
			URL:      "imap+insecure://" + testUser + "@" + addr,
			Password: config.Secret{Env: "IMAPSYNC_PROBE_TEST_BAD_PW"},
		},
	})
	if err == nil {
		t.Fatal("expected authentication to fail")
	}
}

// TestSuggestedConcurrency.
//
// The same count means two different things depending on how the search ended.
// Thirty connections that the server then refused is a wall, and the advice is
// to stay under it. Thirty connections that all succeeded is only the number we
// asked for, and there is nothing there to stay under.
func TestSuggestedConcurrency(t *testing.T) {
	tests := []struct {
		name    string
		ceiling int
		refused bool
		want    int
	}{
		{name: "not measured", ceiling: 0, want: 0},
		{name: "refused at one", ceiling: 1, refused: true, want: 1},
		{name: "refused at two", ceiling: 2, refused: true, want: 1},
		{name: "refused at five", ceiling: 5, refused: true, want: 4},
		{name: "refused at thirty", ceiling: 30, refused: true, want: 29},
		{name: "our own cap of two", ceiling: 2, want: 2},
		{name: "our own cap of thirty", ceiling: 30, want: 30},
		{name: "our own cap of forty-eight", ceiling: 48, want: 48},
	}
	for _, tt := range tests {
		r := probe.Report{MaxConnections: tt.ceiling, Refused: tt.refused}
		if got := r.SuggestedConcurrency(); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}
