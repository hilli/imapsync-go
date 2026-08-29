package compat

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Note is one option that was accepted without doing anything, or refused,
// together with the reason.
type Note struct {
	Option string
	Why    string
}

// Plan is a translated imapsync command line.
type Plan struct {
	// Args is the native command line, starting with the subcommand. It is
	// what runs and what gets printed: one string, so the two cannot drift.
	Args []string

	// Env holds secrets the command line refers to by name rather than by
	// value. A password given as --password1 must not become an argument,
	// because arguments are visible to every process on the machine and a
	// translated command line is the sort of thing people paste into bug
	// reports.
	Env map[string]string

	// Ignored are options accepted without effect, with the reason for each.
	Ignored []Note

	// Assumed are the guesses this had to make, so they can be read and
	// corrected rather than discovered later.
	Assumed []string
}

// RefusedError reports every option that could not be honoured, at once.
//
// All of them, not the first: somebody porting a command line with four
// unsupported flags should learn that in one run rather than four.
type RefusedError struct {
	Refusals []Note
}

func (e *RefusedError) Error() string {
	var sb strings.Builder
	if len(e.Refusals) == 1 {
		sb.WriteString("this imapsync option cannot be honoured:")
	} else {
		fmt.Fprintf(&sb, "%d imapsync options cannot be honoured:", len(e.Refusals))
	}
	for _, r := range e.Refusals {
		fmt.Fprintf(&sb, "\n  %s: %s", r.Option, r.Why)
	}
	return sb.String()
}

// side collects the options describing one end of the copy.
type side struct {
	n                  int // 1 or 2, as imapsync numbers them
	host, user         string
	password, passfile string
	port               int
	ssl, tls           *bool // nil when the command line did not mention it
}

type translator struct {
	src, dst side
	flags    []string
	seen     map[string]bool
	plan     *Plan
	refusals []Note
}

// Translate turns an imapsync command line into this tool's own.
//
// It returns a RefusedError listing everything it could not honour, or a plan
// whose Args are exactly what will run. Nothing is dropped in silence.
func Translate(argv []string) (*Plan, error) {
	givens, err := parse(argv, table())
	if err != nil {
		return nil, err
	}

	t := &translator{
		src:  side{n: 1},
		dst:  side{n: 2},
		seen: map[string]bool{},
		plan: &Plan{Env: map[string]string{}},
	}

	for _, g := range givens {
		out := g.opt.answer(g.on)
		switch out.how {
		case refuse:
			t.refusals = append(t.refusals, Note{Option: spell(g), Why: out.why})
		case ignore:
			t.plan.Ignored = append(t.plan.Ignored, Note{Option: spell(g), Why: out.why})
		case endpoint:
			t.endpoint(g)
		case translate:
			if err := t.emit(g, out); err != nil {
				t.refusals = append(t.refusals, Note{Option: spell(g), Why: err.Error()})
			}
		}
	}

	srcArgs, err := t.side(&t.src, "source")
	if err != nil {
		t.refusals = append(t.refusals, Note{Option: "--host1", Why: err.Error()})
	}
	dstArgs, err := t.side(&t.dst, "dest")
	if err != nil {
		t.refusals = append(t.refusals, Note{Option: "--host2", Why: err.Error()})
	}

	if len(t.refusals) > 0 {
		return nil, &RefusedError{Refusals: t.refusals}
	}

	t.plan.Args = append([]string{"sync"}, srcArgs...)
	t.plan.Args = append(t.plan.Args, dstArgs...)
	t.plan.Args = append(t.plan.Args, t.flags...)
	return t.plan, nil
}

// spell renders an option the way it was typed, so a message about it can be
// searched for in the command line that produced it.
func spell(g given) string {
	if g.opt.kind == boolean && !g.on {
		return "--no" + g.opt.name()
	}
	return "--" + g.opt.name()
}

// emit appends the native flag an option becomes.
func (t *translator) emit(g given, out outcome) error {
	words := strings.Fields(out.native)

	if !g.opt.kind.takesValue() {
		if !g.on && g.opt.off == nil {
			if len(words) != 1 {
				return fmt.Errorf("cannot be turned off")
			}
			words = []string{words[0] + "=false"}
		}
		t.append(words...)
		return nil
	}

	value := g.value
	switch g.opt.name() {
	case "timeout":
		// imapsync counts timeouts in seconds; this tool takes a duration, and
		// a bare number is not one.
		secs, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number of seconds", value)
		}
		value = strconv.FormatFloat(secs, 'f', -1, 64) + "s"
	case "maxage", "minage":
		// imapsync counts ages in days, as a bare number.
		days, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number of days", value)
		}
		value = strconv.FormatFloat(days, 'f', -1, 64) + "d"
	}
	t.append(append(words, value)...)
	return nil
}

// append adds a flag unless the identical flag is already there.
//
// Several imapsync options translate to the same thing — every --debug variant
// asks for debug logging — and repeating it would be harmless but would make
// the echoed command line look like it had been assembled by a machine that
// was not paying attention.
func (t *translator) append(words ...string) {
	key := strings.Join(words, " ")
	if t.seen[key] {
		return
	}
	t.seen[key] = true
	t.flags = append(t.flags, words...)
}

// endpoint files one option under the end of the copy it describes.
func (t *translator) endpoint(g given) {
	name := g.opt.name()
	s := &t.src
	if strings.HasSuffix(name, "2") {
		s = &t.dst
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, "2"), "1")

	switch base {
	case "host":
		s.host = g.value
	case "user":
		s.user = g.value
	case "password":
		s.password = g.value
	case "passfile":
		s.passfile = g.value
	case "port":
		port, err := strconv.Atoi(g.value)
		if err != nil || port < 1 || port > 65535 {
			t.refusals = append(t.refusals, Note{
				Option: spell(g), Why: fmt.Sprintf("%q is not a port number", g.value)})
			return
		}
		s.port = port
	case "ssl":
		on := g.on
		s.ssl = &on
	case "tls":
		on := g.on
		s.tls = &on
	case "office":
		// Documented as exactly this and nothing else, so it can be honoured
		// in full rather than approximated.
		if s.host == "" {
			s.host = "outlook.office365.com"
		}
		if s.ssl == nil {
			on := true
			s.ssl = &on
		}
		t.append("--exclude", "^Files$")
	}
}

// side turns one collected end into the flags that describe it.
func (t *translator) side(s *side, which string) ([]string, error) {
	if s.host == "" {
		return nil, fmt.Errorf("no host given for the %s; imapsync calls it --host%d", which, s.n)
	}
	if s.user == "" {
		return nil, fmt.Errorf("no user given for the %s; imapsync calls it --user%d", which, s.n)
	}

	scheme, port := t.scheme(s)
	if s.port != 0 {
		port = s.port
	}

	u := url.URL{
		Scheme: scheme,
		User:   url.User(s.user),
		Host:   net.JoinHostPort(s.host, strconv.Itoa(port)),
	}
	args := []string{"--" + which + "-url", u.String()}

	switch {
	case s.passfile != "":
		args = append(args, "--"+which+"-password-file", s.passfile)
	case s.password != "":
		// Through the environment, never the command line. See Plan.Env.
		name := "IMAPSYNC_COMPAT_PASSWORD" + strconv.Itoa(s.n)
		t.plan.Env[name] = s.password
		args = append(args, "--"+which+"-password-env", name)
	default:
		return nil, fmt.Errorf("no password given for the %s; imapsync calls it --password%d or --passfile%d",
			which, s.n, s.n)
	}
	return args, nil
}

// scheme decides how to connect, and says so when it had to guess.
//
// imapsync, given neither --ssl nor --tls, probes port 993 and uses TLS if
// something answers. That is a network call in the middle of translating a
// command line, and a translation that depends on what a server was doing at
// the time is not one anybody can reason about. So the explicit answers are
// honoured exactly and the remaining case assumes TLS, which is what the probe
// finds for any server on the public internet — and says it assumed, because
// the alternative is quietly sending somebody's password in the clear.
func (t *translator) scheme(s *side) (string, int) {
	switch {
	case s.ssl != nil && *s.ssl:
		return "imaps", 993
	case s.tls != nil && *s.tls:
		return "imap", 143
	case s.ssl != nil || s.tls != nil:
		// Only negations were given, which is imapsync's way of saying no
		// encryption at all.
		return "imap+insecure", 143
	default:
		t.plan.Assumed = append(t.plan.Assumed, fmt.Sprintf(
			"neither --ssl%d nor --tls%d was given, so TLS on port 993 was assumed; "+
				"say --tls%d for STARTTLS, or --nossl%d for no encryption",
			s.n, s.n, s.n, s.n))
		return "imaps", 993
	}
}

// Explain renders the plan for somebody about to watch it run.
func (p *Plan) Explain(name string) string {
	var sb strings.Builder

	sb.WriteString("translated to:\n  " + name)
	for _, a := range p.Args {
		sb.WriteString(" " + shellQuote(a))
	}
	sb.WriteString("\n")

	for _, a := range p.Assumed {
		sb.WriteString("\nassumed: " + a + "\n")
	}

	if len(p.Ignored) > 0 {
		sorted := make([]Note, len(p.Ignored))
		copy(sorted, p.Ignored)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Option < sorted[j].Option })

		sb.WriteString("\naccepted but did nothing:\n")
		for _, n := range sorted {
			fmt.Fprintf(&sb, "  %-24s %s\n", n.Option, n.Why)
		}
	}
	return sb.String()
}

// shellQuote makes an argument safe to paste back into a shell, which is the
// whole reason for printing the translation.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`*?[]{}()<>|&;#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
