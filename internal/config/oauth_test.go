package config

import (
	"strings"
	"testing"
	"time"
)

// TestAnOAuthBlockNeedsExactlyOneSource. Two sources is the interesting half:
// silently preferring one would mean a run authenticating with a token the
// config's author was not looking at, and a token is the one credential whose
// text tells you nothing about which of the two it came from.
func TestAnOAuthBlockNeedsExactlyOneSource(t *testing.T) {
	t.Parallel()

	both := OAuth{Command: "az account get-access-token", File: "/tmp/token"}
	err := both.Validate()
	if err == nil {
		t.Fatal("an oauth block naming both a command and a file was accepted")
	}
	if !strings.Contains(err.Error(), "command") || !strings.Contains(err.Error(), "file") {
		t.Errorf("Validate() error = %v; it does not name both sources", err)
	}

	if err := (OAuth{}).Validate(); err == nil {
		t.Error("an oauth block naming no source at all was accepted")
	}

	if err := (OAuth{Command: "get-token"}).Validate(); err != nil {
		t.Errorf("a command-only oauth block was rejected: %v", err)
	}
	if err := (OAuth{File: "/tmp/token"}).Validate(); err != nil {
		t.Errorf("a file-only oauth block was rejected: %v", err)
	}
}

// TestATimeoutBesideAFileIsRefused. A timeout bounds a command that hangs.
// Beside a file it has nothing to bound, so it is either a misunderstanding of
// what the file source does or a command that was meant to be there and is not
// -- and both are worth a sentence rather than silence.
func TestATimeoutBesideAFileIsRefused(t *testing.T) {
	t.Parallel()

	withFile := OAuth{File: "/tmp/token", Timeout: 10 * time.Second}
	if err := withFile.Validate(); err == nil {
		t.Error("a timeout beside a file: source was accepted")
	}

	withCommand := OAuth{Command: "get-token", Timeout: 10 * time.Second}
	if err := withCommand.Validate(); err != nil {
		t.Errorf("a timeout beside a command: source was rejected: %v", err)
	}

	if err := (OAuth{Command: "get-token", Timeout: -time.Second}).Validate(); err == nil {
		t.Error("a negative oauth timeout was accepted")
	}
}

// TestAnEndpointTakesAPasswordOrATokenButNotBoth. The alternative to refusing
// is a precedence rule, and a precedence rule here is a silent one: both
// credentials authenticate, so the run succeeds either way and the user never
// learns which of the two the server actually saw.
func TestAnEndpointTakesAPasswordOrATokenButNotBoth(t *testing.T) {
	t.Parallel()

	both := Endpoint{
		URL:      "imaps://user@example.test",
		Password: Secret{Env: "SOME_PASSWORD"},
		OAuth:    OAuth{Command: "get-token"},
	}
	err := both.Validate()
	if err == nil {
		t.Fatal("an endpoint carrying both a password and an oauth block was accepted")
	}
	if !strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "oauth") {
		t.Errorf("Validate() error = %v; it does not name both credentials", err)
	}

	// Either one alone is the ordinary case.
	token := Endpoint{URL: "imaps://user@example.test", OAuth: OAuth{Command: "get-token"}}
	if err := token.Validate(); err != nil {
		t.Errorf("an endpoint authenticating with a token alone was rejected: %v", err)
	}

	// And a bad oauth block is still reported through the endpoint, rather
	// than being validated only when it stands alone.
	bad := Endpoint{URL: "imaps://user@example.test", OAuth: OAuth{Command: "c", File: "f"}}
	if err := bad.Validate(); err == nil {
		t.Error("an endpoint carrying a malformed oauth block was accepted")
	}
}

// TestALocalEndpointTakesNoToken, for the same reason a local endpoint takes
// no password: a credential beside a file:// URL means the URL was edited and
// the credential was not.
func TestALocalEndpointTakesNoToken(t *testing.T) {
	t.Parallel()

	local := Endpoint{URL: "file:///mail", OAuth: OAuth{Command: "get-token"}}
	if err := local.Validate(); err == nil {
		t.Error("a file:// endpoint carrying an oauth block was accepted")
	}
}
