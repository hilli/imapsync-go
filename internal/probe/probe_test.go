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
	if got, want := report.SuggestedConcurrency(), cap-1; got != want {
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

func TestSuggestedConcurrency(t *testing.T) {
	tests := []struct {
		ceiling int
		want    int
	}{
		{ceiling: 0, want: 0},
		{ceiling: 1, want: 1},
		{ceiling: 2, want: 1},
		{ceiling: 5, want: 4},
		{ceiling: 30, want: 29},
	}
	for _, tt := range tests {
		r := probe.Report{MaxConnections: tt.ceiling}
		if got := r.SuggestedConcurrency(); got != tt.want {
			t.Errorf("ceiling %d: got %d, want %d", tt.ceiling, got, tt.want)
		}
	}
}
