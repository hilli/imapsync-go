package imapx

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emersion/go-sasl"
)

// xoauth2Client implements Google's and Microsoft's XOAUTH2 SASL mechanism.
//
// go-sasl ships OAUTHBEARER (RFC 7628) but not XOAUTH2, and the two are not
// interchangeable: neither of the providers this exists for advertises
// OAUTHBEARER on IMAP.
type xoauth2Client struct {
	user  string
	token string

	// challenge holds what the server said when it refused. XOAUTH2 reports
	// failure as a challenge rather than as a refusal, and the challenge is the
	// only place the provider explains itself.
	challenge string
}

func newXOAUTH2Client(user, token string) *xoauth2Client {
	return &xoauth2Client{user: user, token: token}
}

var _ sasl.Client = (*xoauth2Client)(nil)

func (c *xoauth2Client) Start() (string, []byte, error) {
	return string(MechanismXOAUTH2), []byte("user=" + c.user + "\x01auth=Bearer " + c.token + "\x01\x01"), nil
}

// Next is only ever reached when the exchange has already failed: a successful
// XOAUTH2 exchange completes on the initial response alone.
//
// Returning an error here makes go-imap abandon the exchange with the
// connection still mid-command, which is why establish dials afresh for the
// retry rather than reusing this one.
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	c.challenge = string(challenge)
	return nil, fmt.Errorf("server rejected the access token: %s", c.reason())
}

// reason renders the provider's explanation. The challenge is JSON by
// convention rather than by any guarantee, so an unparseable one is reported
// as it arrived instead of being discarded.
func (c *xoauth2Client) reason() string {
	if c.challenge == "" {
		return ""
	}

	var parsed struct {
		Status  string `json:"status"`
		Schemes string `json:"schemes"`
		Scope   string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(c.challenge), &parsed); err != nil {
		return strings.TrimSpace(c.challenge)
	}

	var parts []string
	if parsed.Status != "" {
		parts = append(parts, "status "+parsed.Status)
	}
	if parsed.Scope != "" {
		parts = append(parts, "scope "+parsed.Scope)
	}
	if len(parts) == 0 {
		return strings.TrimSpace(c.challenge)
	}
	return strings.Join(parts, ", ")
}
