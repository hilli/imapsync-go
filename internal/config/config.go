// Package config loads and validates imapsync-go configuration.
//
// Credentials are always referenced indirectly (environment variable, file, or
// OS keychain) and never stored inline in the configuration file.
package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TLSMode describes how the transport to an IMAP server is secured.
type TLSMode string

const (
	// TLSImplicit dials TLS directly, the "imaps" scheme, conventionally port 993.
	TLSImplicit TLSMode = "implicit"
	// TLSStartTLS dials plaintext then upgrades with STARTTLS, conventionally port 143.
	TLSStartTLS TLSMode = "starttls"
	// TLSNone dials plaintext and never upgrades. Test use only.
	TLSNone TLSMode = "none"
)

// FolderMapMode selects the strategy for mapping source folders onto destination folders.
type FolderMapMode string

const (
	// MapSpecialUse resolves well-known folders by their RFC 6154 SPECIAL-USE
	// attribute before falling back to name translation.
	MapSpecialUse FolderMapMode = "special-use"
	// MapName uses name-based rules only.
	MapName FolderMapMode = "name"
	// MapIdentity requires identical names on both sides.
	MapIdentity FolderMapMode = "identity"
)

// Config is the top-level configuration document.
type Config struct {
	Pairs []Pair `yaml:"pairs"`
}

// Pair describes a single source-to-destination synchronisation.
type Pair struct {
	Name        string      `yaml:"name"`
	Source      Endpoint    `yaml:"source"`
	Dest        Endpoint    `yaml:"dest"`
	Concurrency Concurrency `yaml:"concurrency"`
	Folders     Folders     `yaml:"folders"`
	Delete2     bool        `yaml:"delete2"`
}

// Endpoint is one side of a pair.
type Endpoint struct {
	URL      string `yaml:"url"`
	Password Secret `yaml:"password"`
}

// Secret references a credential held outside the configuration file. Exactly
// one source must be set.
type Secret struct {
	Env      string `yaml:"env"`
	File     string `yaml:"file"`
	Keychain string `yaml:"keychain"`
}

// Concurrency bounds the connection pools and in-flight memory for a pair.
type Concurrency struct {
	Source      Limit    `yaml:"source"`
	Dest        Limit    `yaml:"dest"`
	MaxInflight ByteSize `yaml:"max_inflight"`
}

// Folders controls which folders are synchronised and how they are mapped.
type Folders struct {
	Map     FolderMapMode `yaml:"map"`
	Include []string      `yaml:"include"`
	Exclude []string      `yaml:"exclude"`
	Rules   []MapRule     `yaml:"rules"`
}

// MapRule is an explicit source-to-destination folder override, applied after
// SPECIAL-USE and namespace translation.
type MapRule struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Limit is a connection count that may be automatically tuned. A zero value
// means "auto": the adaptive governor chooses the working concurrency.
type Limit int

// Auto reports whether the limit is left to the adaptive governor.
func (l Limit) Auto() bool { return l == 0 }

// UnmarshalYAML accepts either the string "auto" or a non-negative integer.
func (l *Limit) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("concurrency limit must be \"auto\" or an integer: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(raw), "auto") {
		*l = 0
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("concurrency limit %q is neither \"auto\" nor an integer", raw)
	}
	if n < 0 {
		return fmt.Errorf("concurrency limit must not be negative, got %d", n)
	}
	*l = Limit(n)
	return nil
}

// ByteSize is a quantity of bytes accepting suffixed forms such as "512MiB".
type ByteSize int64

var byteUnits = []struct {
	suffix string
	scale  int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
	{"B", 1},
}

// ParseByteSize parses a byte quantity such as "512MiB", "1GB" or "1048576".
func ParseByteSize(s string) (ByteSize, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(s))
	if trimmed == "" {
		return 0, errors.New("empty byte size")
	}
	for _, u := range byteUnits {
		if !strings.HasSuffix(trimmed, u.suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(trimmed, u.suffix))
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
		}
		if v < 0 {
			return 0, fmt.Errorf("byte size must not be negative: %q", s)
		}
		return ByteSize(v * float64(u.scale)), nil
	}
	v, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	return ByteSize(v), nil
}

// UnmarshalYAML parses either a bare integer or a suffixed string.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("invalid byte size: %w", err)
	}
	size, err := ParseByteSize(raw)
	if err != nil {
		return err
	}
	*b = size
	return nil
}

// Address is a parsed endpoint URL.
type Address struct {
	Host string
	Port int
	User string
	TLS  TLSMode
}

// HostPort renders the address for dialling.
func (a Address) HostPort() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// Address parses the endpoint URL into its dialling components.
//
// Supported schemes:
//
//	imaps://user@host[:port]           implicit TLS, default port 993
//	imap://user@host[:port]            STARTTLS,     default port 143
//	imap+insecure://user@host[:port]   plaintext,    default port 143
//
// The username is taken literally up to the last "@" in the authority, so an
// email address needs no percent-encoding:
//
//	imaps://apple@example.com@imap.mail.me.com
//
// Percent-encoded usernames are still accepted and decoded, so the equivalent
// imaps://apple%40example.com@imap.mail.me.com also works.
func (e Endpoint) Address() (Address, error) {
	raw := strings.TrimSpace(e.URL)
	if raw == "" {
		return Address{}, errors.New("url is required")
	}

	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return Address{}, errors.New("missing scheme, expected scheme://user@host")
	}

	var addr Address
	switch strings.ToLower(scheme) {
	case "imaps":
		addr.TLS, addr.Port = TLSImplicit, 993
	case "imap":
		addr.TLS, addr.Port = TLSStartTLS, 143
	case "imap+insecure":
		addr.TLS, addr.Port = TLSNone, 143
	default:
		return Address{}, fmt.Errorf("unsupported scheme %q, want imaps, imap or imap+insecure", scheme)
	}

	// A mailbox path or query string would be silently ignored, so reject it
	// rather than connect to something the user did not ask for.
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		if strings.Trim(rest[i:], "/") != "" {
			return Address{}, errors.New("url must not contain a path or query, expected scheme://user@host[:port]")
		}
		rest = rest[:i]
	}

	// Split on the LAST "@": everything before it is the username, which lets an
	// unencoded email address through unharmed.
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return Address{}, errors.New("missing username, expected scheme://user@host")
	}
	userinfo, hostport := rest[:at], rest[at+1:]

	if userinfo == "" {
		return Address{}, errors.New("missing username, expected scheme://user@host")
	}
	if strings.Contains(userinfo, ":") {
		return Address{}, errors.New("url contains an inline password; reference it via password.env, password.file or password.keychain instead")
	}
	addr.User = userinfo
	if strings.Contains(userinfo, "%") {
		if decoded, err := url.PathUnescape(userinfo); err == nil {
			addr.User = decoded
		}
	}

	host, port, err := splitHostPort(hostport)
	if err != nil {
		return Address{}, err
	}
	addr.Host = host
	if port != 0 {
		addr.Port = port
	}

	return addr, nil
}

// splitHostPort splits "host", "host:port", "[::1]" or "[::1]:port". A zero
// port means none was given, leaving the scheme default in place.
func splitHostPort(hostport string) (host string, port int, err error) {
	if hostport == "" {
		return "", 0, errors.New("missing host")
	}

	var portStr string
	if strings.HasPrefix(hostport, "[") {
		end := strings.Index(hostport, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("unterminated IPv6 address in %q", hostport)
		}
		host = hostport[1:end]
		switch remainder := hostport[end+1:]; {
		case remainder == "":
		case strings.HasPrefix(remainder, ":"):
			portStr = remainder[1:]
		default:
			return "", 0, fmt.Errorf("unexpected %q after IPv6 address", remainder)
		}
	} else if h, p, found := strings.Cut(hostport, ":"); found {
		host, portStr = h, p
	} else {
		host = hostport
	}

	if host == "" {
		return "", 0, errors.New("missing host")
	}
	if portStr == "" {
		return host, 0, nil
	}

	n, err := strconv.Atoi(portStr)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, n, nil
}

// Resolve returns the secret value from its configured source. The context
// bounds the keychain lookup, which is the one source that can block
// indefinitely: it may put a prompt in front of the user.
func (s Secret) Resolve(ctx context.Context) (string, error) {
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", s.Env)
		}
		return v, nil

	case s.File != "":
		raw, err := os.ReadFile(s.File)
		if err != nil {
			return "", fmt.Errorf("reading secret file: %w", err)
		}
		v := strings.TrimRight(string(raw), "\r\n")
		if v == "" {
			return "", fmt.Errorf("secret file %s is empty", s.File)
		}
		return v, nil

	case s.Keychain != "":
		if runtime.GOOS != "darwin" {
			return "", fmt.Errorf("keychain secrets are only supported on macOS, not %s", runtime.GOOS)
		}
		// The keychain item name comes from the user's own configuration, and
		// the argument vector is passed to exec without a shell, so there is
		// nothing here for a metacharacter to escape into.
		out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", s.Keychain, "-w").Output() // #nosec G204
		if err != nil {
			return "", fmt.Errorf("reading keychain item %q: %w", s.Keychain, err)
		}
		v := strings.TrimRight(string(out), "\r\n")
		if v == "" {
			return "", fmt.Errorf("keychain item %q is empty", s.Keychain)
		}
		return v, nil

	default:
		return "", errors.New("no secret source configured, set one of env, file or keychain")
	}
}

// Validate checks that exactly one secret source is configured.
func (s Secret) Validate() error {
	n := 0
	for _, set := range []bool{s.Env != "", s.File != "", s.Keychain != ""} {
		if set {
			n++
		}
	}
	switch n {
	case 1:
		return nil
	case 0:
		return errors.New("no secret source configured, set one of env, file or keychain")
	default:
		return errors.New("multiple secret sources configured, set exactly one of env, file or keychain")
	}
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	// Reading the file the user named is the whole purpose of this function.
	raw, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(raw)
}

// Parse decodes and validates a configuration document.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

const defaultMaxInflight = ByteSize(512 << 20)

func (c *Config) applyDefaults() {
	for i := range c.Pairs {
		p := &c.Pairs[i]
		if p.Folders.Map == "" {
			p.Folders.Map = MapSpecialUse
		}
		if p.Concurrency.MaxInflight == 0 {
			p.Concurrency.MaxInflight = defaultMaxInflight
		}
	}
}

// Validate reports the first structural problem found in the configuration.
func (c *Config) Validate() error {
	if len(c.Pairs) == 0 {
		return errors.New("config defines no pairs")
	}
	seen := make(map[string]struct{}, len(c.Pairs))
	for i := range c.Pairs {
		p := &c.Pairs[i]
		if p.Name == "" {
			return fmt.Errorf("pair %d has no name", i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate pair name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		sides := []struct {
			label string
			ep    Endpoint
		}{{"source", p.Source}, {"dest", p.Dest}}
		for _, side := range sides {
			if _, err := side.ep.Address(); err != nil {
				return fmt.Errorf("pair %q %s: %w", p.Name, side.label, err)
			}
			if err := side.ep.Password.Validate(); err != nil {
				return fmt.Errorf("pair %q %s password: %w", p.Name, side.label, err)
			}
		}

		switch p.Folders.Map {
		case MapSpecialUse, MapName, MapIdentity:
		default:
			return fmt.Errorf("pair %q: unknown folder map mode %q", p.Name, p.Folders.Map)
		}
	}
	return nil
}

// Pair returns the named pair.
func (c *Config) Pair(name string) (*Pair, error) {
	for i := range c.Pairs {
		if c.Pairs[i].Name == name {
			return &c.Pairs[i], nil
		}
	}
	return nil, fmt.Errorf("no pair named %q", name)
}
