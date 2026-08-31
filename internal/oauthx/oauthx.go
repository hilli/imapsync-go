// Package oauthx performs the two OAuth exchanges an IMAP client needs: a
// one-time consent that yields a refresh token, and the repeated exchange of
// that refresh token for an access token.
//
// It deliberately implements no provider knowledge beyond the endpoints it is
// given. Google and Microsoft differ in which fields they populate, not in the
// protocol, and a table of provider quirks here would be a table of guesses
// about servers we cannot all test against.
package oauthx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultHTTPTimeout bounds a single token request. The exchange is one small
// POST to a large provider; a request still outstanding after this is not
// going to succeed, and a migration must not stall on it.
const DefaultHTTPTimeout = 30 * time.Second

// Credentials is everything needed to keep minting access tokens, and is the
// document written by Login and read by the refresh path.
//
// It is deliberately the shape of Google's authorized_user file so that a
// credential minted by other tooling can be used unchanged.
type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token"`
	TokenURI     string `json:"token_uri"`

	// Account is what the token authenticates as, carried so a stored
	// credential can say which mailbox it belongs to. Nothing depends on it.
	Account string `json:"account,omitempty"`
}

// Validate reports whether the credential can be exchanged at all.
func (c Credentials) Validate() error {
	switch {
	case c.ClientID == "":
		return errors.New("client_id is missing")
	case c.RefreshToken == "":
		return errors.New("refresh_token is missing")
	case c.TokenURI == "":
		// Not defaulted: this is where the refresh token gets sent.
		return errors.New("token_uri is missing")
	default:
		return nil
	}
}

// ParseCredentials reads the JSON document a Secret resolved to.
func ParseCredentials(blob string) (Credentials, error) {
	var c Credentials
	if err := json.Unmarshal([]byte(blob), &c); err != nil {
		return Credentials{}, fmt.Errorf("parsing the oauth credential: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Credentials{}, fmt.Errorf("the oauth credential is incomplete: %w", err)
	}
	return c, nil
}

// Token is one access token and whatever the provider said alongside it.
type Token struct {
	AccessToken string
	// RefreshToken is set only when the provider rotated it.
	RefreshToken string
	ExpiresIn    int
}

// Error is a provider's own refusal, kept whole because its error code is the
// only thing that distinguishes "try again" from "start over".
type Error struct {
	Code        string
	Description string
	Status      int
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Description != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	case e.Code != "":
		return e.Code
	case e.Description != "":
		return e.Description
	default:
		return fmt.Sprintf("the provider refused with HTTP %d", e.Status)
	}
}

// Dead reports whether the refresh token will never work again, so the only
// useful advice is to run the consent step afresh. Retrying an invalid_grant
// produces the same answer for ever.
func (e *Error) Dead() bool {
	return e.Code == "invalid_grant"
}

// Exchange performs one form-encoded token request and reads the result.
func Exchange(ctx context.Context, client *http.Client, tokenURI string, form url.Values) (Token, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("building the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("asking %s for a token: %w", tokenURI, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded because the body is a small JSON document and a provider having
	// a bad day should not be able to exhaust memory here.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("reading the token response: %w", err)
	}

	var parsed struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	// A provider that answered with HTML rather than JSON has still said
	// something, and its status code is worth reporting on its own.
	_ = json.Unmarshal(body, &parsed)

	if resp.StatusCode != http.StatusOK || parsed.Error != "" {
		return Token{}, &Error{
			Code:        parsed.Error,
			Description: parsed.ErrorDescription,
			Status:      resp.StatusCode,
		}
	}
	if parsed.AccessToken == "" {
		return Token{}, errors.New("the provider returned no access token and no error")
	}

	return Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresIn:    parsed.ExpiresIn,
	}, nil
}

// RefreshAccessToken trades a refresh token for an access token.
func RefreshAccessToken(ctx context.Context, client *http.Client, c Credentials) (Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {c.ClientID},
	}
	// Google issues desktop clients a secret and requires it here. Microsoft
	// public clients have none, and sending an empty one is an error rather
	// than a no-op.
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	return Exchange(ctx, client, c.TokenURI, form)
}

// pkce is a verifier and the challenge derived from it. Microsoft requires
// PKCE of public clients and Google recommends it, so it is not optional.
type pkce struct {
	verifier  string
	challenge string
}

func newPKCE() (pkce, error) {
	v, err := randomString(64)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(v))
	return pkce{
		verifier:  v,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// randomString returns n bytes of randomness in an unpadded URL-safe encoding,
// which is what both the verifier and the state value need.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
