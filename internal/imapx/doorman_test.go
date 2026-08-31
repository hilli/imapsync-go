package imapx_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// doorman puts an XOAUTH2 exchange in front of an honest server that has never
// heard of it.
//
// The in-memory server speaks LOGIN and nothing else, and scripting a whole
// server would only ever test the script. So the proxy answers AUTHENTICATE
// itself and, once it is satisfied, logs in upstream on the client's behalf --
// leaving every other command to a real implementation. It is the same trick
// that caught iCloud's phantom SEARCH UIDs, and it buys the one behaviour no
// real provider will perform on cue: accept a token, then start refusing it,
// in the middle of a run.
type doorman struct {
	addr string

	// accepted is the token the door opens for. Swapping it mid-run is how a
	// token is made to expire.
	mu       sync.Mutex
	accepted string

	// saslIR advertises SASL-IR, which decides whether the client sends its
	// token on the AUTHENTICATE line or waits to be asked for it.
	saslIR bool

	// seen records every token offered, in order, so a test can tell one
	// attempt from two.
	seenMu sync.Mutex
	seen   []string

	// exchanges counts completed AUTHENTICATE commands, refused included.
	exchanges atomic.Int32

	// conns counts accepted connections. Every authentication attempt should
	// arrive on one of its own.
	conns atomic.Int32

	upstreamUser, upstreamPass string
}

func startDoorman(t *testing.T, upstream, user, pass, accepted string, saslIR bool) *doorman {
	t.Helper()

	d := &doorman{accepted: accepted, saslIR: saslIR, upstreamUser: user, upstreamPass: pass}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	d.addr = ln.Addr().String()

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(client, upstream)
		}
	}()
	return d
}

// expire makes the door start refusing what it has been accepting.
func (d *doorman) expire(next string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accepted = next
}

func (d *doorman) tokensSeen() []string {
	d.seenMu.Lock()
	defer d.seenMu.Unlock()
	return append([]string(nil), d.seen...)
}

func (d *doorman) serve(client net.Conn, upstream string) {
	d.conns.Add(1)
	defer func() { _ = client.Close() }()

	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()

	toClient := bufio.NewWriter(client)
	fromClient := bufio.NewReader(client)
	toServer := bufio.NewWriter(server)
	fromServer := bufio.NewReader(server)

	send := func(format string, a ...any) bool {
		_, _ = fmt.Fprintf(toClient, format+"\r\n", a...)
		return toClient.Flush() == nil
	}

	// The greeting, with the mechanism the upstream cannot offer.
	greeting, err := fromServer.ReadString('\n')
	if err != nil {
		return
	}
	if !send("%s", strings.TrimRight(d.advertise(greeting), "\r\n")) {
		return
	}

	for {
		line, err := fromClient.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])

		if cmd != "AUTHENTICATE" {
			// Anything before authentication that is not the exchange goes
			// upstream, and its answer comes back with the capability line
			// rewritten to match what the greeting promised.
			_, _ = toServer.WriteString(line)
			if toServer.Flush() != nil {
				return
			}
			if !d.relayUntilTagged(tag, fromServer, toClient) {
				return
			}
			continue
		}

		if !d.admit(tag, fields, fromClient, send) {
			return
		}

		// Satisfied: log in upstream so the rest of the session is genuine.
		_, _ = fmt.Fprintf(toServer, "prx LOGIN %s %s\r\n", d.upstreamUser, d.upstreamPass)
		if toServer.Flush() != nil {
			return
		}
		if !d.consumeTagged("prx", fromServer) {
			return
		}
		if !send("%s OK authenticated", tag) {
			return
		}

		// From here the proxy has no further opinions.
		go func() { _, _ = io.Copy(server, fromClient) }()
		_, _ = io.Copy(client, fromServer)
		return
	}
}

// admit runs the AUTHENTICATE exchange and reports whether the token was
// accepted. A refusal is deliberately left mid-exchange: XOAUTH2 reports it as
// a challenge, so the server is still waiting for a response the abandoning
// client never sends.
func (d *doorman) admit(tag string, fields []string, fromClient *bufio.Reader, send func(string, ...any) bool) bool {
	// The mechanism name is part of what a provider is being asked to accept,
	// so the door checks it rather than assuming it.
	if len(fields) < 3 || strings.ToUpper(fields[2]) != "XOAUTH2" {
		send("%s NO Unsupported authentication mechanism", tag)
		return false
	}

	payload := ""
	if len(fields) >= 4 {
		// tag AUTHENTICATE MECHANISM <initial response>
		payload = fields[3]
	} else { // No initial response: the client waits to be asked.
		if !send("+") {
			return false
		}
		answer, err := fromClient.ReadString('\n')
		if err != nil {
			return false
		}
		payload = strings.TrimSpace(answer)
	}

	d.exchanges.Add(1)
	_, token, decodeErr := decodeXOAUTH2(payload)
	if decodeErr != nil {
		send("%s BAD %v", tag, decodeErr)
		return false
	}

	d.seenMu.Lock()
	d.seen = append(d.seen, token)
	d.seenMu.Unlock()

	d.mu.Lock()
	ok := token == d.accepted
	d.mu.Unlock()

	if !ok {
		// XOAUTH2 refuses with a challenge rather than a refusal, and the
		// challenge is where the provider explains itself.
		challenge := base64.StdEncoding.EncodeToString(
			[]byte(`{"status":"401","schemes":"Bearer","scope":"https://mail.google.com/"}`))
		if !send("+ %s", challenge) {
			return false
		}
		send("%s NO Invalid credentials (Failure)", tag)
		return false
	}
	return true
}

// advertise adds XOAUTH2, and optionally SASL-IR, to a capability line.
func (d *doorman) advertise(line string) string {
	if !strings.Contains(line, "CAPABILITY") {
		return line
	}
	extra := "CAPABILITY AUTH=XOAUTH2 "
	if d.saslIR {
		extra = "CAPABILITY AUTH=XOAUTH2 SASL-IR "
	}
	return strings.Replace(line, "CAPABILITY ", extra, 1)
}

func (d *doorman) relayUntilTagged(tag string, from *bufio.Reader, to *bufio.Writer) bool {
	for {
		line, err := from.ReadString('\n')
		if line != "" {
			if _, werr := to.WriteString(d.advertise(line)); werr != nil {
				return false
			}
			if to.Flush() != nil {
				return false
			}
			if strings.HasPrefix(line, tag+" ") {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}

func (d *doorman) consumeTagged(tag string, from *bufio.Reader) bool {
	for {
		line, err := from.ReadString('\n')
		if strings.HasPrefix(line, tag+" ") {
			return strings.Contains(strings.ToUpper(line), " OK")
		}
		if err != nil {
			return false
		}
	}
}

// decodeXOAUTH2 takes the initial response apart, insisting on the exact
// framing the providers specify rather than merely finding the token in it.
func decodeXOAUTH2(payload string) (user, token string, err error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", "", fmt.Errorf("payload is not base64: %w", err)
	}

	s := string(raw)
	if !strings.HasSuffix(s, "\x01\x01") {
		return "", "", fmt.Errorf("payload does not end with two \\x01")
	}
	parts := strings.Split(strings.TrimSuffix(s, "\x01\x01"), "\x01")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("payload has %d parts, want 2", len(parts))
	}
	if !strings.HasPrefix(parts[0], "user=") {
		return "", "", fmt.Errorf("first part is %q, want user=", parts[0])
	}
	if !strings.HasPrefix(parts[1], "auth=Bearer ") {
		return "", "", fmt.Errorf("second part is %q, want auth=Bearer ", parts[1])
	}
	return strings.TrimPrefix(parts[0], "user="), strings.TrimPrefix(parts[1], "auth=Bearer "), nil
}
