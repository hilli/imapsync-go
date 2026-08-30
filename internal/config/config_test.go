package config

import (
	"strings"
	"testing"
)

func TestEndpointAddress(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantHost string
		wantPort int
		wantUser string
		wantTLS  TLSMode
		wantErr  string
	}{
		{
			name:     "imaps defaults to 993",
			url:      "imaps://you@imap.mail.me.com",
			wantHost: "imap.mail.me.com", wantPort: 993, wantUser: "you", wantTLS: TLSImplicit,
		},
		{
			name:     "imap defaults to 143 with starttls",
			url:      "imap://you@mox.example.net",
			wantHost: "mox.example.net", wantPort: 143, wantUser: "you", wantTLS: TLSStartTLS,
		},
		{
			name:     "explicit port wins",
			url:      "imaps://you@example.com:1993",
			wantHost: "example.com", wantPort: 1993, wantUser: "you", wantTLS: TLSImplicit,
		},
		{
			name:     "insecure scheme",
			url:      "imap+insecure://you@127.0.0.1:1143",
			wantHost: "127.0.0.1", wantPort: 1143, wantUser: "you", wantTLS: TLSNone,
		},
		{
			name:     "percent-encoded email address as username",
			url:      "imaps://you%40example.com@imap.example.com",
			wantHost: "imap.example.com", wantPort: 993, wantUser: "you@example.com", wantTLS: TLSImplicit,
		},
		{
			name:     "unencoded email address as username",
			url:      "imaps://apple@hilli.dk@imap.mail.me.com",
			wantHost: "imap.mail.me.com", wantPort: 993, wantUser: "apple@hilli.dk", wantTLS: TLSImplicit,
		},
		{
			name:     "unencoded email address with explicit port",
			url:      "imaps://apple@hilli.dk@imap.mail.me.com:993",
			wantHost: "imap.mail.me.com", wantPort: 993, wantUser: "apple@hilli.dk", wantTLS: TLSImplicit,
		},
		{
			name:     "ipv6 host without port",
			url:      "imap+insecure://you@[::1]",
			wantHost: "::1", wantPort: 143, wantUser: "you", wantTLS: TLSNone,
		},
		{
			name:     "ipv6 host with port",
			url:      "imap+insecure://you@[::1]:1143",
			wantHost: "::1", wantPort: 1143, wantUser: "you", wantTLS: TLSNone,
		},
		{
			name:     "trailing slash tolerated",
			url:      "imaps://you@example.com/",
			wantHost: "example.com", wantPort: 993, wantUser: "you", wantTLS: TLSImplicit,
		},
		{name: "rejects inline password", url: "imaps://you:hunter2@example.com", wantErr: "inline password"},
		{name: "rejects missing user", url: "imaps://example.com", wantErr: "missing username"},
		{name: "rejects unknown scheme", url: "pop3://you@example.com", wantErr: "unsupported scheme"},
		{name: "rejects empty url", url: "", wantErr: "url is required"},
		{name: "rejects bad port", url: "imaps://you@example.com:99999", wantErr: "invalid port"},
		{name: "rejects missing scheme", url: "you@example.com", wantErr: "missing scheme"},
		{name: "rejects empty username", url: "imaps://@example.com", wantErr: "missing username"},
		{name: "rejects missing host", url: "imaps://you@", wantErr: "missing host"},
		{name: "rejects a mailbox path", url: "imaps://you@example.com/INBOX", wantErr: "must not contain a path"},
		{name: "rejects unterminated ipv6", url: "imaps://you@[::1", wantErr: "unterminated IPv6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, err := Endpoint{URL: tt.url}.Address()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", addr.Host, tt.wantHost)
			}
			if addr.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", addr.Port, tt.wantPort)
			}
			if addr.User != tt.wantUser {
				t.Errorf("user = %q, want %q", addr.User, tt.wantUser)
			}
			if addr.TLS != tt.wantTLS {
				t.Errorf("tls = %q, want %q", addr.TLS, tt.wantTLS)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in      string
		want    ByteSize
		wantErr bool
	}{
		{in: "512MiB", want: 512 << 20},
		{in: "1GiB", want: 1 << 30},
		{in: "1GB", want: 1000 * 1000 * 1000},
		{in: "64K", want: 64 << 10},
		{in: "1048576", want: 1048576},
		{in: "1.5MiB", want: ByteSize(1.5 * float64(1<<20))},
		{in: " 8 MiB ", want: 8 << 20},
		{in: "", wantErr: true},
		{in: "-1MiB", wantErr: true},
		{in: "banana", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseByteSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %d", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSecretValidate(t *testing.T) {
	tests := []struct {
		name    string
		secret  Secret
		wantErr string
	}{
		{name: "env only", secret: Secret{Env: "PW"}},
		{name: "file only", secret: Secret{File: "/tmp/pw"}},
		{name: "keychain only", secret: Secret{Keychain: "svc"}},
		{name: "none", secret: Secret{}, wantErr: "no secret source"},
		{name: "two", secret: Secret{Env: "PW", File: "/tmp/pw"}, wantErr: "multiple secret sources"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.secret.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got none", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}

func TestSecretResolveFromEnv(t *testing.T) {
	t.Setenv("IMAPSYNC_TEST_PW", "hunter2")

	got, err := Secret{Env: "IMAPSYNC_TEST_PW"}.Resolve(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("got %q, want %q", got, "hunter2")
	}

	if _, err := (Secret{Env: "IMAPSYNC_TEST_PW_MISSING"}).Resolve(t.Context()); err == nil {
		t.Error("expected an error for an unset variable")
	}
}

const goodConfig = `
pairs:
  - name: icloud-to-mox
    source:
      url: "imaps://you@imap.mail.me.com:993"
      password: {env: ICLOUD_APP_PW}
    dest:
      url: "imaps://you@mox.example.net:993"
      password: {keychain: mox-imap}
    concurrency: {source: auto, dest: 8, max_inflight: 512MiB}
    folders:
      map: special-use
      exclude: ["Notes"]
    delete2: false
`

func TestParseGoodConfig(t *testing.T) {
	cfg, err := Parse([]byte(goodConfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Pairs) != 1 {
		t.Fatalf("got %d pairs, want 1", len(cfg.Pairs))
	}

	p := cfg.Pairs[0]
	if !p.Concurrency.Source.Auto() {
		t.Error("source concurrency should be auto")
	}
	if p.Concurrency.Dest != 8 {
		t.Errorf("dest concurrency = %d, want 8", p.Concurrency.Dest)
	}
	if p.Concurrency.MaxInflight != 512<<20 {
		t.Errorf("max_inflight = %d, want %d", p.Concurrency.MaxInflight, ByteSize(512<<20))
	}
	if p.Folders.Map != MapSpecialUse {
		t.Errorf("folder map = %q, want %q", p.Folders.Map, MapSpecialUse)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
pairs:
  - name: minimal
    source: {url: "imaps://a@one.example", password: {env: A}}
    dest: {url: "imaps://b@two.example", password: {env: B}}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p := cfg.Pairs[0]
	if p.Folders.Map != MapSpecialUse {
		t.Errorf("default folder map = %q, want %q", p.Folders.Map, MapSpecialUse)
	}
	if p.Concurrency.MaxInflight != defaultMaxInflight {
		t.Errorf("default max_inflight = %d, want %d", p.Concurrency.MaxInflight, defaultMaxInflight)
	}
	if !p.Concurrency.Source.Auto() || !p.Concurrency.Dest.Auto() {
		t.Error("concurrency should default to auto")
	}
}

func TestParseRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "no pairs",
			yaml:    "pairs: []",
			wantErr: "no pairs",
		},
		{
			name:    "unnamed pair",
			yaml:    "pairs:\n  - source: {url: \"imaps://a@one.example\", password: {env: A}}\n    dest: {url: \"imaps://b@two.example\", password: {env: B}}\n",
			wantErr: "has no name",
		},
		{
			name:    "duplicate names",
			yaml:    "pairs:\n  - {name: dup, source: {url: \"imaps://a@one.example\", password: {env: A}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}}\n  - {name: dup, source: {url: \"imaps://a@one.example\", password: {env: A}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}}\n",
			wantErr: "duplicate pair name",
		},
		{
			name:    "inline password in url",
			yaml:    "pairs:\n  - {name: p, source: {url: \"imaps://a:pw@one.example\", password: {env: A}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}}\n",
			wantErr: "inline password",
		},
		{
			name:    "no secret source",
			yaml:    "pairs:\n  - {name: p, source: {url: \"imaps://a@one.example\", password: {}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}}\n",
			wantErr: "no secret source",
		},
		{
			name:    "unknown folder map mode",
			yaml:    "pairs:\n  - {name: p, source: {url: \"imaps://a@one.example\", password: {env: A}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}, folders: {map: telepathy}}\n",
			wantErr: "unknown folder map mode",
		},
		{
			name:    "unknown field",
			yaml:    "pairs:\n  - {name: p, sauce: nope, source: {url: \"imaps://a@one.example\", password: {env: A}}, dest: {url: \"imaps://b@two.example\", password: {env: B}}}\n",
			wantErr: "field sauce not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}

// TestExampleConfigParses keeps the shipped example honest: if the schema
// changes and the example is not updated, this fails.
func TestExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../imapsync.example.yaml")
	if err != nil {
		t.Fatalf("example config does not parse: %v", err)
	}
	if len(cfg.Pairs) == 0 {
		t.Fatal("example config defines no pairs")
	}
}
