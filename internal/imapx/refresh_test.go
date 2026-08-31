package imapx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hilli/imapsync-go/internal/oauthx"
)

// A provider that rotates the refresh token on every exchange, as both real
// ones are free to do, and records what it was presented with.
type rotatingProvider struct {
	mu        sync.Mutex
	presented []string
	issued    int
	dead      bool
}

func (p *rotatingProvider) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	p.mu.Lock()
	p.presented = append(p.presented, r.Form.Get("refresh_token"))
	p.issued++
	n := p.issued
	dead := p.dead
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if dead {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Token has been expired or revoked.",
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "access-" + string(rune('0'+n)),
		"refresh_token": "refresh-" + string(rune('0'+n)),
		"expires_in":    3600,
	})
}

func (p *rotatingProvider) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.presented...)
}

// The provider may hand back a new refresh token on any exchange, and
// presenting the superseded one eventually stops working. A migration runs for
// hours and re-mints repeatedly, so the second exchange must use what the
// first was given.
func TestARotatedRefreshTokenIsCarriedForward(t *testing.T) {
	t.Parallel()

	provider := &rotatingProvider{}
	server := httptest.NewServer(http.HandlerFunc(provider.serve))
	defer server.Close()

	cred := RefreshToken(oauthx.Credentials{
		ClientID:     "id",
		RefreshToken: "original",
		TokenURI:     server.URL,
	}, 0)

	first, err := cred.Secret(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}

	second, ok, err := cred.Refresh(context.Background(), first)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if !ok {
		t.Fatal("the credential declined to re-mint after a refusal")
	}

	if first == second {
		t.Errorf("the refused token was handed out again: %q", first)
	}

	seen := provider.seen()
	if len(seen) != 2 {
		t.Fatalf("the provider was asked %d times, want 2", len(seen))
	}
	if seen[0] != "original" {
		t.Errorf("the first exchange presented %q, want the stored token", seen[0])
	}
	if seen[1] != "refresh-1" {
		t.Errorf("the second exchange presented %q, want the token the first was given", seen[1])
	}
}

// A revoked consent cannot be recovered from by retrying, and the only thing
// that fixes it is a human at a browser. Say so, rather than reporting a
// generic refusal that reads like a network problem.
func TestARevokedConsentSaysWhatToDoAboutIt(t *testing.T) {
	t.Parallel()

	provider := &rotatingProvider{dead: true}
	server := httptest.NewServer(http.HandlerFunc(provider.serve))
	defer server.Close()

	cred := RefreshToken(oauthx.Credentials{
		ClientID: "id", RefreshToken: "revoked", TokenURI: server.URL,
	}, 0)

	_, err := cred.Secret(context.Background())
	if err == nil {
		t.Fatal("a revoked credential produced a token")
	}
	if !strings.Contains(err.Error(), "oauth login") {
		t.Errorf("the error does not say the consent must be redone: %v", err)
	}
}
