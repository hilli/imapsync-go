// Package compat translates an imapsync command line into this tool's own.
//
// The point is not to accept imapsync's flags. It is to be honest about them.
// imapsync has some two hundred and thirty options and this tool implements a
// fraction of them, so the interesting question for every flag is not "can we
// spell it" but "what happens to somebody's mail if we pretend we understood".
//
// Three answers, and every option gets exactly one. A flag with an equivalent
// is translated. A flag asking for something this tool already does, or for
// something that cannot mean anything here, is accepted and reported as
// ignored, with the reason. Anything else — anything that would change which
// messages move, or what becomes of the ones that do not — is refused, because
// dropping it silently would sync the wrong mailbox and then report success.
//
// Unknown options are refused for the same reason. A typo that parses is worse
// than one that does not.
package compat

import (
	"fmt"
	"sort"
	"strings"
)

// valueKind is what an option expects after it, in Getopt::Long's notation.
type valueKind int

const (
	// noValue is a bare option: 'abort'. No argument, and no negated form.
	noValue valueKind = iota
	// boolean is 'dry!': no argument, but --nodry and --no-dry are accepted.
	boolean
	// text is '=s'.
	text
	// textList is '=s@', which may be repeated.
	textList
	// textPairs is '=s%', a key=value pair which may be repeated.
	textPairs
	// integer is '=i'.
	integer
	// number is '=f'.
	number
)

// takesValue reports whether an option must be followed by an argument.
func (k valueKind) takesValue() bool { return k >= text }

// option is one entry in imapsync's option table.
type option struct {
	// names holds every spelling, the canonical one first. Getopt::Long writes
	// these with '|' between them.
	names []string
	kind  valueKind

	// on is what this option means, and is what the rest of the package is
	// actually for. See table.go.
	on outcome
	// off is what the negated spelling means, when that differs.
	off *outcome
}

func (o *option) name() string { return o.names[0] }

// given is one option as it appeared on the command line, resolved to the
// table entry it named.
type given struct {
	opt   *option
	on    bool   // for noValue and boolean: false only for a negated spelling
	value string // for the kinds that take one
}

// target is what a spelling resolves to. Two spellings meaning the same option
// with the same polarity are not an ambiguity, which is why this gets compared
// rather than the spelling itself.
type target struct {
	opt *option
	on  bool
}

// index maps every accepted spelling, lowercased, onto what it means.
//
// Lowercased because Getopt::Long matches case-insensitively unless bundling is
// switched on, which imapsync does not do: --DRY and --FIXinboxinbox both work
// against the real thing, and a command line that worked yesterday has to work
// today.
func index(opts []*option) map[string]target {
	idx := make(map[string]target, len(opts)*3)
	for _, o := range opts {
		for _, n := range o.names {
			lower := strings.ToLower(n)
			idx[lower] = target{opt: o, on: true}
			if o.kind == boolean {
				// Getopt::Long registers both negated spellings for an option
				// declared with '!', and they take part in abbreviation like
				// any other: --nod is ambiguous between nodry and nodry1.
				idx["no"+lower] = target{opt: o, on: false}
				idx["no-"+lower] = target{opt: o, on: false}
			}
		}
	}
	return idx
}

// resolve turns one spelling into the option it names.
//
// Exact match first, then unique abbreviation, which is Getopt::Long's
// auto_abbrev and is on by default. An abbreviation matching several spellings
// of the same option with the same polarity is not ambiguous — --expung
// prefixes both 'expunge1' and its alias 'expunge' and resolves fine against
// the real thing — so what has to be counted is distinct meanings, not
// distinct spellings.
func resolve(idx map[string]target, spelling string) (target, error) {
	key := strings.ToLower(spelling)
	if t, ok := idx[key]; ok {
		return t, nil
	}

	var (
		found     target
		seen      bool
		ambiguous bool
		matched   []string
	)
	for k, t := range idx {
		if !strings.HasPrefix(k, key) {
			continue
		}
		matched = append(matched, k)
		switch {
		case !seen:
			found, seen = t, true
		case t != found:
			ambiguous = true
		}
	}

	switch {
	case ambiguous:
		sort.Strings(matched)
		return target{}, fmt.Errorf("option --%s is ambiguous: it could be %s",
			spelling, strings.Join(dashed(matched), ", "))
	case seen:
		return found, nil
	default:
		return target{}, fmt.Errorf("unknown option --%s", spelling)
	}
}

func dashed(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "--" + n
	}
	return out
}

// parse reads an imapsync command line.
//
// The rules are Getopt::Long's, checked against it rather than remembered: one
// dash or two, --opt=value or --opt value, a mandatory value swallowing the
// next argument even when that argument looks like an option, -- ending the
// options, and an option declared with '!' refusing a value outright rather
// than reading --dry=0 as a negation.
func parse(args []string, opts []*option) ([]given, error) {
	idx := index(opts)

	var (
		out   []given
		extra []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" {
			extra = append(extra, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			extra = append(extra, a)
			continue
		}

		body := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		spelling, value, hasValue := strings.Cut(body, "=")

		t, err := resolve(idx, spelling)
		if err != nil {
			return nil, err
		}

		if !t.opt.kind.takesValue() {
			if hasValue {
				return nil, fmt.Errorf("option --%s does not take an argument", spelling)
			}
			out = append(out, given{opt: t.opt, on: t.on})
			continue
		}

		if !hasValue {
			rest := args[i+1:]
			if len(rest) == 0 {
				return nil, fmt.Errorf("option --%s requires an argument", spelling)
			}

			// Getopt::Long takes the next argument whatever it looks like, so
			// --host1 --dry sets host1 to the string "--dry" rather than
			// complaining. Verified against Perl; see getoptlong_test.go.
			value = rest[0]
			i++
		}
		out = append(out, given{opt: t.opt, on: true, value: value})
	}

	if len(extra) > 0 {
		return nil, fmt.Errorf("imapsync takes no arguments of its own, but got %q",
			strings.Join(extra, " "))
	}
	return out, nil
}
