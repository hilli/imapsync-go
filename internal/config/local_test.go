package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestALocalEndpointIsRecognisedByItsScheme pins the one question every call
// site asks first. Getting it wrong in the permissive direction sends a
// password to a directory; in the strict direction it tries to open a TCP
// connection to a path.
func TestALocalEndpointIsRecognisedByItsScheme(t *testing.T) {
	t.Parallel()

	for url, want := range map[string]bool{
		"file:///Users/hilli/Mail":  true,
		"FILE:///Users/hilli/Mail":  true,
		"  file://backup  ":         true,
		"imaps://user@example.test": false,
		"imap+insecure://u@h":       false,
		"":                          false,
		// A path that merely mentions the word is still a server.
		"imaps://file@example.test": false,
	} {
		if got := (Endpoint{URL: url}).IsLocal(); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", url, got, want)
		}
	}
}

// TestLocalPathTakesThePathLiterally is the decision that a directory named
// "Rejsen 50%" is a directory named "Rejsen 50%".
//
// A file: URL would percent-decode, and then a user with a real "100%" folder
// could not name it, while "a%20b" would mean two different directories
// depending on which tool read it.
func TestLocalPathTakesThePathLiterally(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	for name, tc := range map[string]struct{ url, want string }{
		"absolute":     {"file:///Users/hilli/Mail", "/Users/hilli/Mail"},
		"a per cent":   {"file:///mail/Rejsen 50%", "/mail/Rejsen 50%"},
		"not decoded":  {"file:///mail/a%20b", "/mail/a%20b"},
		"relative":     {"file://backup", filepath.Join(wd, "backup")},
		"home":         {"file://~/Mail", filepath.Join(home, "Mail")},
		"home alone":   {"file://~", home},
		"surrounded":   {"  file:///mail  ", "/mail"},
		"upper scheme": {"FILE:///mail", "/mail"},
	} {
		got, err := (Endpoint{URL: tc.url}).LocalPath()
		if err != nil {
			t.Errorf("%s: LocalPath(%q) error = %v", name, tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: LocalPath(%q) = %q, want %q", name, tc.url, got, tc.want)
		}
	}
}

// TestLocalPathRefusesWhatItCannotMeanFaithfully. A query or fragment in a
// path is far more likely to be a URL habit than a directory called "x?y", and
// silently making a directory of the whole string would put mail somewhere the
// user did not ask for.
func TestLocalPathRefusesWhatItCannotMeanFaithfully(t *testing.T) {
	t.Parallel()

	for name, url := range map[string]string{
		"no path":    "file://",
		"a query":    "file:///mail?limit=1",
		"a fragment": "file:///mail#inbox",
		"not local":  "imaps://user@example.test",
	} {
		if got, err := (Endpoint{URL: url}).LocalPath(); err == nil {
			t.Errorf("%s: LocalPath(%q) = %q, want an error", name, url, got)
		}
	}
}

// TestAddressSaysWhyALocalEndpointHasNoAddress. Six call sites reach for
// Address, and the generic parse failure it used to give ("missing scheme", or
// worse, a host called "") would send someone looking for a typo in a URL that
// is correct.
func TestAddressSaysWhyALocalEndpointHasNoAddress(t *testing.T) {
	t.Parallel()

	_, err := (Endpoint{URL: "file:///mail"}).Address()
	if err == nil {
		t.Fatal("Address() on a file:// endpoint succeeded")
	}
	if !strings.Contains(err.Error(), "file://") {
		t.Errorf("Address() error = %v; it does not mention the scheme that caused it", err)
	}
}

// TestALocalEndpointTakesNoPassword. Failing loudly matters more than it looks:
// a password in a config beside a file:// URL almost always means the URL was
// edited and the credential was not, and accepting it silently would open a
// directory while the user believes they are talking to a server.
func TestALocalEndpointTakesNoPassword(t *testing.T) {
	t.Parallel()

	local := Endpoint{URL: "file:///mail"}
	if err := local.Validate(); err != nil {
		t.Errorf("a bare file:// endpoint was rejected: %v", err)
	}

	withPassword := Endpoint{URL: "file:///mail", Password: Secret{Env: "SOME_PASSWORD"}}
	if err := withPassword.Validate(); err == nil {
		t.Error("a file:// endpoint carrying a password was accepted")
	}

	// And a server endpoint still has to have one.
	if err := (Endpoint{URL: "imaps://user@example.test"}).Validate(); err == nil {
		t.Error("a server endpoint with no password was accepted")
	}
}

// TestDescribeDoesNotRenameExistingPairs is a migration test wearing a small
// disguise.
//
// Describe is what names a migration in the state database. If it returns
// anything other than what the previous code returned for a server, every
// existing state database stops matching its own run, and a tool whose whole
// job is not copying mail twice copies all of it twice.
func TestDescribeDoesNotRenameExistingPairs(t *testing.T) {
	t.Parallel()

	ep := Endpoint{URL: "imaps://jens@imap.mail.me.com"}
	addr, err := ep.Address()
	if err != nil {
		t.Fatalf("Address() error = %v", err)
	}
	want := addr.User + "@" + addr.Host

	got, err := ep.Describe()
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if got != want {
		t.Errorf("Describe() = %q, want the unchanged %q", got, want)
	}

	// A local endpoint has to be distinguishable from a server, and from
	// another directory.
	one, err := (Endpoint{URL: "file:///mail/a"}).Describe()
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	two, err := (Endpoint{URL: "file:///mail/b"}).Describe()
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if one == two {
		t.Errorf("two directories describe alike: %q", one)
	}
	if one == want {
		t.Errorf("a directory describes like a server: %q", one)
	}

	// The same directory written two ways is the same migration, or a run
	// started from a different working directory would start over.
	rel, err := (Endpoint{URL: "file:///mail/./a/"}).Describe()
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if rel != one {
		t.Errorf("the same directory described two ways: %q and %q", one, rel)
	}
}
