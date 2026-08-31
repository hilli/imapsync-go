package oauthx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider is an OAuth server that checks what we send rather than merely
// answering. A provider that accepted anything would let a malformed request
// pass every test and fail only against Google.
type fakeProvider struct {
	mu sync.Mutex

	// challenge is what the authorization request claimed, kept so the token
	// request's verifier can be checked against it.
	challenge string

	// refreshTokens are handed out in order, so a test can rotate one.
	refreshTokens []string
	accessTokens  []string

	// refuse, when set, is returned instead of a token.
	refuse string

	seenForms []url.Values
	srv       *httptest.Server
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()

	p := &fakeProvider{
		refreshTokens: []string{"refresh-1"},
		accessTokens:  []string{"access-1"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.challenge = r.URL.Query().Get("code_challenge")
		p.mu.Unlock()
		http.Error(w, "the test drives the redirect itself", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		p.mu.Lock()
		defer p.mu.Unlock()
		p.seenForms = append(p.seenForms, r.PostForm)

		if p.refuse != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + p.refuse + `","error_description":"it will not work again"}`))
			return
		}

		if r.PostForm.Get("grant_type") == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"PKCE verifier does not match"}`))
				return
			}
		}

		body := map[string]any{"access_token": p.next(&p.accessTokens), "expires_in": 3599}
		if rt := p.next(&p.refreshTokens); rt != "" {
			body["refresh_token"] = rt
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})

	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// next pops the head of a list, repeating the last entry for ever after.
func (p *fakeProvider) next(list *[]string) string {
	if len(*list) == 0 {
		return ""
	}
	v := (*list)[0]
	if len(*list) > 1 {
		*list = (*list)[1:]
	}
	return v
}

func (p *fakeProvider) client() Client {
	return Client{
		ID:       "client-id",
		Secret:   "client-secret",
		AuthURL:  p.srv.URL + "/authorize",
		TokenURL: p.srv.URL + "/token",
		Scopes:   []string{"https://mail.google.com/"},
	}
}

func (p *fakeProvider) forms() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.seenForms...)
}

// browser stands in for the user: it reads the authorization URL, then calls
// back to the loopback listener exactly as a real browser would be redirected.
func browser(t *testing.T, authURL string, mangle func(url.Values) url.Values) {
	t.Helper()

	u, err := url.Parse(authURL)
	if err != nil {
		t.Errorf("the authorization URL does not parse: %v", err)
		return
	}
	q := u.Query()

	// Tell the provider what challenge was claimed, which is what a real
	// authorization request would have done.
	if _, err := http.Get(authURL); err != nil { //nolint:bodyclose,gosec // the fake answers 501; the URL is the test's own
		t.Logf("priming the authorization endpoint: %v", err)
	}

	back := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
	if mangle != nil {
		back = mangle(back)
	}

	resp, err := http.Get(q.Get("redirect_uri") + "?" + back.Encode()) //nolint:gosec // a loopback URL this test just created
	if err != nil {
		t.Logf("calling back to the listener: %v", err)
		return
	}
	_ = resp.Body.Close()
}

// TestLoginReturnsAStorableCredential is the whole consent, end to end, with
// the browser replaced and nothing else.
func TestLoginReturnsAStorableCredential(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	var shown string

	got, err := Login(context.Background(), p.client(), LoginOptions{
		Prompt:      func(u string) { shown = u },
		OpenBrowser: func(_ context.Context, u string) error { go browser(t, u, nil); return nil },
		Timeout:     20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if got.RefreshToken != "refresh-1" {
		t.Errorf("refresh token = %q; want the one the provider issued", got.RefreshToken)
	}
	if got.TokenURI != p.client().TokenURL {
		t.Errorf("token_uri = %q; a stored credential that cannot say where to refresh is useless", got.TokenURI)
	}
	if got.ClientID != "client-id" || got.ClientSecret != "client-secret" {
		t.Errorf("credential = %+v; the client is not carried, so a refresh could not be reconstructed", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Login produced a credential it would itself reject: %v", err)
	}

	if !strings.Contains(shown, "code_challenge_method=S256") {
		t.Errorf("authorization URL = %q; PKCE is required by Microsoft, not optional", shown)
	}
	if !strings.Contains(shown, "access_type=offline") {
		t.Errorf("authorization URL = %q; without offline access Google issues no refresh token", shown)
	}
}

// TestLoginRefusesARedirectWithTheWrongState. The listener is on loopback,
// where any local process can reach it, so the state value is what ties a
// redirect to the login that started it.
func TestLoginRefusesARedirectWithTheWrongState(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	_, err := Login(context.Background(), p.client(), LoginOptions{
		OpenBrowser: func(_ context.Context, u string) error {
			go browser(t, u, func(v url.Values) url.Values {
				v.Set("state", "not-the-state-we-issued")
				return v
			})
			return nil
		},
		Timeout: 20 * time.Second,
	})
	if err == nil {
		t.Fatal("a redirect carrying a forged state value was accepted")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("error = %v; it does not say what was wrong", err)
	}
	if len(p.forms()) != 0 {
		t.Error("a forged redirect still reached the token endpoint")
	}
}

// TestLoginSendsThePKCEVerifier proves the verifier matches the challenge, by
// having the provider check it rather than by inspecting our own request.
func TestLoginSendsThePKCEVerifier(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	if _, err := Login(context.Background(), p.client(), LoginOptions{
		OpenBrowser: func(_ context.Context, u string) error { go browser(t, u, nil); return nil },
		Timeout:     20 * time.Second,
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	forms := p.forms()
	if len(forms) != 1 {
		t.Fatalf("the token endpoint saw %d requests; want 1", len(forms))
	}
	if forms[0].Get("code_verifier") == "" {
		t.Error("the code exchange carried no PKCE verifier")
	}
	if forms[0].Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", forms[0].Get("grant_type"))
	}
}

// TestLoginSaysSoWhenNoRefreshTokenComesBack. Storing an access token alone
// would produce a credential that works for an hour and then cannot be
// renewed -- the exact failure this feature exists to prevent, arriving later.
func TestLoginSaysSoWhenNoRefreshTokenComesBack(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	p.refreshTokens = nil

	_, err := Login(context.Background(), p.client(), LoginOptions{
		OpenBrowser: func(_ context.Context, u string) error { go browser(t, u, nil); return nil },
		Timeout:     20 * time.Second,
	})
	if err == nil {
		t.Fatal("a consent that produced no refresh token was accepted")
	}
	if !strings.Contains(err.Error(), "refresh token") {
		t.Errorf("error = %v; it does not name what was missing", err)
	}
}

// TestRefreshAccessTokenTradesTheStoredToken covers the ordinary path and the
// rotation the providers may do at any time.
func TestRefreshAccessTokenTradesTheStoredToken(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	p.accessTokens = []string{"access-A", "access-B"}
	p.refreshTokens = []string{"refresh-1", "refresh-2"}

	creds := Credentials{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RefreshToken: "stored-refresh",
		TokenURI:     p.srv.URL + "/token",
	}

	tok, err := RefreshAccessToken(context.Background(), nil, creds)
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if tok.AccessToken != "access-A" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "refresh-1" {
		t.Errorf("rotated refresh token = %q; a caller that ignores this is locked out later", tok.RefreshToken)
	}

	form := p.forms()[0]
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != "stored-refresh" {
		t.Errorf("refresh_token = %q; want the stored one", form.Get("refresh_token"))
	}
	if form.Get("client_secret") != "client-secret" {
		t.Errorf("client_secret = %q; Google requires it for a desktop client", form.Get("client_secret"))
	}
}

// TestAnEmptyClientSecretIsOmitted. Microsoft public clients have no secret,
// and an empty one is a malformed request rather than an absent field.
func TestAnEmptyClientSecretIsOmitted(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	creds := Credentials{
		ClientID:     "client-id",
		RefreshToken: "stored-refresh",
		TokenURI:     p.srv.URL + "/token",
	}
	if _, err := RefreshAccessToken(context.Background(), nil, creds); err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}

	if _, present := p.forms()[0]["client_secret"]; present {
		t.Error("an empty client_secret was sent as a parameter rather than left out")
	}
}

// TestADeadRefreshTokenIsDistinguishable. Everything else is worth retrying;
// invalid_grant is not, and the only useful advice is to consent again.
func TestADeadRefreshTokenIsDistinguishable(t *testing.T) {
	t.Parallel()

	p := newFakeProvider(t)
	p.refuse = "invalid_grant"

	_, err := RefreshAccessToken(context.Background(), nil, Credentials{
		ClientID: "c", RefreshToken: "r", TokenURI: p.srv.URL + "/token",
	})
	if err == nil {
		t.Fatal("a refused refresh succeeded")
	}

	var provider *Error
	if !errors.As(err, &provider) {
		t.Fatalf("error = %T; callers cannot tell a dead token from a bad day", err)
	}
	if !provider.Dead() {
		t.Error("invalid_grant did not report itself as permanent")
	}
	if !strings.Contains(err.Error(), "it will not work again") {
		t.Errorf("error = %v; the provider's own words are missing", err)
	}

	p.refuse = "temporarily_unavailable"
	_, err = RefreshAccessToken(context.Background(), nil, Credentials{
		ClientID: "c", RefreshToken: "r", TokenURI: p.srv.URL + "/token",
	})
	if !errors.As(err, &provider) || provider.Dead() {
		t.Errorf("error = %v; a transient refusal was reported as permanent", err)
	}
}

// TestParseCredentialsRefusesAnIncompleteDocument. Each missing field fails
// later and less clearly: no token_uri means nowhere to ask, and a missing
// refresh token means a credential that cannot renew.
func TestParseCredentialsRefusesAnIncompleteDocument(t *testing.T) {
	t.Parallel()

	full := `{"client_id":"c","client_secret":"s","refresh_token":"r","token_uri":"https://example.test/token"}`
	got, err := ParseCredentials(full)
	if err != nil {
		t.Fatalf("a complete credential was rejected: %v", err)
	}
	if got.ClientID != "c" || got.RefreshToken != "r" || got.TokenURI != "https://example.test/token" {
		t.Errorf("parsed = %+v", got)
	}

	for name, blob := range map[string]string{
		"no client_id":     `{"refresh_token":"r","token_uri":"https://example.test/token"}`,
		"no refresh_token": `{"client_id":"c","token_uri":"https://example.test/token"}`,
		"no token_uri":     `{"client_id":"c","refresh_token":"r"}`,
		"not json":         `this is a password, not a credential document`,
	} {
		if _, err := ParseCredentials(blob); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestOpenBrowserRefusesANonHTTPURL. The URL reaches a platform opener, and on
// a desktop those hand file:// and custom schemes to whatever claims them.
func TestOpenBrowserRefusesANonHTTPURL(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"file:///etc/passwd", "javascript:alert(1)", "ms-msdt:/id"} {
		if err := OpenBrowser(context.Background(), target); err == nil {
			t.Errorf("OpenBrowser(%q) was allowed", target)
		}
	}
}

// A migration is usually run from a browser signed in to a different account,
// and consenting as the wrong mailbox produces a credential that authenticates
// cleanly and then syncs nothing.
func TestTheAccountHintReachesTheProvider(t *testing.T) {
	t.Parallel()

	client := Client{
		ID:       "id",
		AuthURL:  "https://accounts.example.test/o/oauth2/auth",
		TokenURL: "https://example.test/token",
		Scopes:   []string{"https://mail.google.com/"},
	}

	got, err := AuthCodeURL(client, "http://127.0.0.1:1/", "state", "challenge", "someone@example.com")
	if err != nil {
		t.Fatalf("building the URL: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if hint := parsed.Query().Get("login_hint"); hint != "someone@example.com" {
		t.Errorf("login_hint is %q, want the account asked for", hint)
	}

	// And absent rather than empty when there is nothing to hint, since an
	// empty one is a parameter the provider has to interpret.
	got, err = AuthCodeURL(client, "http://127.0.0.1:1/", "state", "challenge", "")
	if err != nil {
		t.Fatalf("building the URL: %v", err)
	}
	parsed, _ = url.Parse(got)
	if _, present := parsed.Query()["login_hint"]; present {
		t.Error("an empty login_hint was sent")
	}
}
