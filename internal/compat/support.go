package compat

//go:generate go run ../docsgen

// This file exposes the option table as data, so the support documentation can
// be generated from the thing that actually runs rather than written beside it.
//
// A hand-kept matrix of 200-odd options is wrong the first time an option moves
// from refused to translated and nobody remembers the doc exists. Generating it
// means the two cannot disagree, and the staleness test means a change that
// forgets to regenerate fails rather than ships.

// Status is what the shim does with an imapsync option.
type Status string

const (
	// StatusTranslated means the option becomes a native flag.
	StatusTranslated Status = "translated"
	// StatusEndpoint means the option is folded into --source-url or
	// --dest-url, which several options build together.
	StatusEndpoint Status = "endpoint"
	// StatusIgnored means the option is accepted and does nothing, with a
	// reason. It asks for what already happens, or for something that cannot
	// mean anything here.
	StatusIgnored Status = "ignored"
	// StatusRefused means the run stops and says why.
	StatusRefused Status = "refused"
)

// Value describes what an option expects after it.
type Value string

// The value kinds, in the words the documentation uses rather than
// Getopt::Long's punctuation.
const (
	ValueNone     Value = ""
	ValueBoolean  Value = "on/off"
	ValueText     Value = "string"
	ValueTextList Value = "string (repeatable)"
	ValuePairs    Value = "key=value (repeatable)"
	ValueInteger  Value = "integer"
	ValueNumber   Value = "number"
)

// Support is one imapsync option as this tool treats it.
type Support struct {
	// Name is the canonical spelling; Aliases are the rest, if any.
	Name    string
	Aliases []string
	Value   Value

	Status Status
	Native string // the native flag, when Status is StatusTranslated
	Why    string // the reason, when Status is StatusIgnored or StatusRefused

	// NegatedStatus, NegatedNative and NegatedWhy describe the --no… spelling
	// when it differs from the plain one. Several options are harmless in one
	// direction and not in the other: --syncinternaldates asks for what
	// already happens, while --nosyncinternaldates asks for something this
	// tool cannot do.
	NegatedStatus Status
	NegatedNative string
	NegatedWhy    string
}

// Negates reports whether the option has a --no… spelling that means something
// different from the plain one.
func (s Support) Negates() bool { return s.NegatedStatus != "" }

// Options returns every imapsync option the shim knows, in the order
// imapsync's own GetOptions calls declare them.
func Options() []Support {
	entries := table()
	out := make([]Support, 0, len(entries))
	for _, o := range entries {
		s := Support{
			Name:  o.names[0],
			Value: valueOf(o.kind),
		}
		if len(o.names) > 1 {
			s.Aliases = append([]string(nil), o.names[1:]...)
		}
		s.Status, s.Native, s.Why = describe(o.on)
		if o.off != nil {
			s.NegatedStatus, s.NegatedNative, s.NegatedWhy = describe(*o.off)
		}
		out = append(out, s)
	}
	return out
}

func describe(on outcome) (Status, string, string) {
	switch on.how {
	case translate:
		return StatusTranslated, on.native, ""
	case endpoint:
		return StatusEndpoint, "", ""
	case ignore:
		return StatusIgnored, "", on.why
	case refuse:
		return StatusRefused, "", on.why
	}
	// refuse is the zero value and is covered above; anything else is a
	// disposition added without teaching this function about it.
	return StatusRefused, "", on.why
}

func valueOf(k valueKind) Value {
	switch k {
	case boolean:
		return ValueBoolean
	case text:
		return ValueText
	case textList:
		return ValueTextList
	case textPairs:
		return ValuePairs
	case integer:
		return ValueInteger
	case number:
		return ValueNumber
	case noValue:
		return ValueNone
	}
	return ValueNone
}
