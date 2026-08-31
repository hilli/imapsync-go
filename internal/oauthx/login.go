package oauthx

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Client is the application a consent is performed as. Google's console hands
// this over as a file; Microsoft's does not, so every field can also be given
// explicitly.
type Client struct {
	ID       string
	Secret   string
	AuthURL  string
	TokenURL string
	Scopes   []string
}

// Validate reports whether a consent can be attempted.
func (c Client) Validate() error {
	switch {
	case c.ID == "":
		return errors.New("no client id")
	case c.AuthURL == "":
		return errors.New("no authorization endpoint")
	case c.TokenURL == "":
		return errors.New("no token endpoint")
	case len(c.Scopes) == 0:
		return errors.New("no scopes, so the token would grant nothing")
	default:
		return nil
	}
}

// LoginOptions carries the things a caller may want to see or control.
type LoginOptions struct {
	// Prompt receives the URL to visit. A caller that cannot open a browser
	// prints it; the default opens one.
	Prompt func(url string)

	// OpenBrowser is called with the authorization URL. Nil uses the
	// platform's opener. Returning an error is not fatal -- the URL has
	// already been shown, and a user can paste it.
	OpenBrowser func(ctx context.Context, url string) error

	// HTTPClient is used for the code exchange. Nil builds a bounded one.
	HTTPClient *http.Client

	// Timeout bounds how long to wait for the user to finish consenting.
	// Zero uses DefaultConsentTimeout.
	Timeout time.Duration

	// LoginHint names the mailbox to consent as. Only a hint: the account
	// chooser still appears, and the provider is free to ignore it. Worth
	// sending because a migration is usually run by someone signed in to a
	// different account in that browser.
	LoginHint string
}

// DefaultConsentTimeout is how long the listener waits for a human. Consent
// involves choosing an account and reading a warning screen, so it is generous;
// but it must end, because a listener bound to a port for ever is a surprise.
const DefaultConsentTimeout = 5 * time.Minute

// Login performs the loopback consent described in RFC 8252 and returns a
// credential that can mint access tokens indefinitely.
//
// Loopback rather than a device code because it is the one flow both providers
// support for the mail scopes: Google's limited-input flow does not cover
// https://mail.google.com/, so a device code path would serve only Microsoft
// and leave two flows to maintain. A headless machine is served by consenting
// on a workstation and copying the resulting document, which is why that
// document is the stored format.
func Login(ctx context.Context, c Client, opts LoginOptions) (Credentials, error) {
	if err := c.Validate(); err != nil {
		return Credentials{}, err
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultConsentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Port zero: the kernel picks. Both providers accept any port on loopback
	// for a desktop client, so pinning one would only invite a clash with
	// whatever else is running on a migration box.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return Credentials{}, fmt.Errorf("listening for the consent redirect: %w", err)
	}
	defer func() { _ = listener.Close() }()

	redirect := fmt.Sprintf("http://127.0.0.1:%d/", listener.Addr().(*net.TCPAddr).Port)

	verifier, err := newPKCE()
	if err != nil {
		return Credentials{}, err
	}
	state, err := randomString(32)
	if err != nil {
		return Credentials{}, err
	}

	authURL, err := AuthCodeURL(c, redirect, state, verifier.challenge, opts.LoginHint)
	if err != nil {
		return Credentials{}, err
	}

	if opts.Prompt != nil {
		opts.Prompt(authURL)
	}
	open := opts.OpenBrowser
	if open == nil {
		open = OpenBrowser
	}
	// Deliberately ignored: the URL has been shown, so a machine with no
	// browser is inconvenient rather than stuck.
	_ = open(ctx, authURL)

	code, err := waitForCode(ctx, listener, state)
	if err != nil {
		return Credentials{}, err
	}

	token, err := Exchange(ctx, opts.HTTPClient, c.TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {c.ID},
		"client_secret": secretValues(c.Secret),
		"code_verifier": {verifier.verifier},
	})
	if err != nil {
		return Credentials{}, err
	}
	if token.RefreshToken == "" {
		// Without one there is nothing to store, and a run would work until
		// the access token expired and then stop. Google withholds it when the
		// account has consented before, which its own docs answer with
		// prompt=consent -- already set in AuthCodeURL.
		return Credentials{}, errors.New("the provider returned no refresh token, so nothing could be stored; " +
			"revoke this application's access on the account and consent again")
	}

	return Credentials{
		ClientID:     c.ID,
		ClientSecret: c.Secret,
		RefreshToken: token.RefreshToken,
		TokenURI:     c.TokenURL,
	}, nil
}

// secretValues omits an empty secret rather than sending a blank one, which
// Microsoft treats as a malformed public-client request.
func secretValues(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// AuthCodeURL builds the URL the user visits to consent.
func AuthCodeURL(c Client, redirect, state, challenge, hint string) (string, error) {
	u, err := url.Parse(c.AuthURL)
	if err != nil {
		return "", fmt.Errorf("parsing the authorization endpoint: %w", err)
	}

	q := u.Query()
	q.Set("client_id", c.ID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(c.Scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// Google issues a refresh token only when both are present, and only on
	// the first consent unless asked again.
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	if hint != "" {
		q.Set("login_hint", hint)
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// waitForCode serves exactly one redirect and returns the code it carried.
func waitForCode(ctx context.Context, listener net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()

			// Constant time because the comparison is what stops another
			// local process racing a forged redirect into this listener.
			got, want := q.Get("state"), state
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				reply(w, http.StatusBadRequest, "That redirect did not come from this login. Nothing was stored.")
				select {
				case results <- result{err: errors.New("the consent redirect carried the wrong state value")}:
				default:
				}
				return
			}

			if e := q.Get("error"); e != "" {
				reply(w, http.StatusBadRequest, "Consent was refused. You can close this window.")
				select {
				case results <- result{err: &Error{Code: e, Description: q.Get("error_description")}}:
				default:
				}
				return
			}

			code := q.Get("code")
			if code == "" {
				reply(w, http.StatusBadRequest, "That redirect carried no authorization code.")
				select {
				case results <- result{err: errors.New("the consent redirect carried no authorization code")}:
				default:
				}
				return
			}

			reply(w, http.StatusOK, "Consent complete. You can close this window and return to the terminal.")
			select {
			case results <- result{code: code}:
			default:
			}
		}),
	}

	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for consent: %w", ctx.Err())
	case res := <-results:
		return res.code, res.err
	}
}

func reply(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

// OpenBrowser asks the desktop to open a URL.
//
// The context bounds the opener, not the browser. All three openers hand the
// URL to a running desktop and exit, so by the time a consent finishes -- a
// human deciding, minutes later -- the process being cancelled here is long
// gone. What the context protects against is the opposite case: an opener that
// hangs because there is no desktop to hand to.
func OpenBrowser(ctx context.Context, target string) error {
	// Parsed rather than passed through, so that nothing but an http(s) URL
	// can reach a shell-adjacent opener.
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("parsing the URL to open: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing to open a %q URL", u.Scheme)
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", u.String()) //nolint:gosec // an argument vector with no shell, and u is parsed and confirmed http(s) above
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u.String()) //nolint:gosec // an argument vector with no shell, and u is parsed and confirmed http(s) above
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", u.String()) //nolint:gosec // an argument vector with no shell, and u is parsed and confirmed http(s) above
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped rather than left: nothing waits for the result, and an unwaited
	// child stays a zombie for the life of a process that may run for hours.
	go func() { _ = cmd.Wait() }()
	return nil
}

// clientFile is the shape the Google console downloads. Microsoft's console
// offers nothing equivalent, which is why every field is also a flag.
type clientFile struct {
	Installed *clientFileBody `json:"installed"`
	Web       *clientFileBody `json:"web"`
}

type clientFileBody struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
}

// ParseClientFile reads a downloaded client credential.
//
// Both shapes are accepted because the console decides which key it writes
// from the application type chosen months earlier, and a user who picked "Web
// application" gets a document that differs only in that word. The consent
// flow here is the same either way; what a web client actually lacks is
// permission to use a loopback redirect, and the provider will say so far more
// clearly than a parse error would.
func ParseClientFile(blob []byte) (Client, error) {
	var file clientFile
	if err := json.Unmarshal(blob, &file); err != nil {
		return Client{}, fmt.Errorf("parsing the client file: %w", err)
	}

	body := file.Installed
	if body == nil {
		body = file.Web
	}
	if body == nil {
		return Client{}, errors.New("the client file has neither an \"installed\" nor a \"web\" section, " +
			"so it is not the JSON the console hands out")
	}

	return Client{
		ID:       body.ClientID,
		Secret:   body.ClientSecret,
		AuthURL:  body.AuthURI,
		TokenURL: body.TokenURI,
	}, nil
}
