package compat

import (
	"errors"
	"strings"
	"testing"
)

// The expectations here were taken from Getopt::Long itself rather than from
// its documentation: a Perl script declaring the same options was run against
// each of these command lines and its answers copied down. Two of them are not
// what you would guess. --dry=0 is an error rather than a negation, and
// --expung is unambiguous even though it prefixes two different spellings,
// because both spell the same option.
func TestArgumentsAreReadTheWayGetoptLongReadsThem(t *testing.T) {
	t.Parallel()

	opts := []*option{
		tr("dry!", "--dry-run"),
		ig("dry1!", "x"),
		ep("host1=s"),
		tr("folder=s@", "--folder"),
		ig("expunge1|expunge!", "x"),
		ig("fixInboxINBOX!", "x"),
	}

	tests := []struct {
		name string
		args []string
		want []string // "option=value" or "option=on"/"option=off"
		err  string
	}{
		{name: "a long option", args: []string{"--dry"}, want: []string{"dry=on"}},
		{name: "a single dash", args: []string{"-host1", "h"}, want: []string{"host1=h"}},
		{name: "an equals sign", args: []string{"--host1=h"}, want: []string{"host1=h"}},
		{name: "negation", args: []string{"--nodry"}, want: []string{"dry=off"}},
		{name: "negation with a dash", args: []string{"--no-dry"}, want: []string{"dry=off"}},
		{name: "case is ignored", args: []string{"--DRY"}, want: []string{"dry=on"}},
		{name: "case is ignored in a mixed-case option", args: []string{"--FIXinboxinbox"}, want: []string{"fixInboxINBOX=on"}},
		{name: "an unambiguous abbreviation", args: []string{"--fold", "A"}, want: []string{"folder=A"}},
		{name: "an alias", args: []string{"--expunge"}, want: []string{"expunge1=on"}},
		{name: "an abbreviation of two spellings of one option", args: []string{"--expung"}, want: []string{"expunge1=on"}},
		{name: "a repeated option", args: []string{"--folder", "A", "--folder", "B"}, want: []string{"folder=A", "folder=B"}},
		{
			name: "a mandatory value swallows what follows it",
			args: []string{"--host1", "-x"},
			want: []string{"host1=-x"},
		},

		{name: "an ambiguous abbreviation", args: []string{"--dr"}, err: "ambiguous"},
		{name: "an ambiguous negation", args: []string{"--nod"}, err: "ambiguous"},
		{name: "a value on an option that takes none", args: []string{"--dry=0"}, err: "does not take an argument"},
		{name: "a missing value", args: []string{"--host1"}, err: "requires an argument"},
		{name: "an option that does not exist", args: []string{"--bogus"}, err: "unknown option"},
		{name: "an argument that is not an option", args: []string{"you@example.test"}, err: "takes no arguments"},
		{name: "an argument after the terminator", args: []string{"--dry", "--", "x"}, err: "takes no arguments"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parse(tc.args, opts)
			if tc.err != "" {
				if err == nil {
					t.Fatalf("parse(%q) succeeded, want an error about %q", tc.args, tc.err)
				}
				if !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parse(%q) error = %v, want it to mention %q", tc.args, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) error = %v", tc.args, err)
			}

			var lines []string
			for _, g := range got {
				switch {
				case g.opt.kind.takesValue():
					lines = append(lines, g.opt.name()+"="+g.value)
				case g.on:
					lines = append(lines, g.opt.name()+"=on")
				default:
					lines = append(lines, g.opt.name()+"=off")
				}
			}
			if strings.Join(lines, ",") != strings.Join(tc.want, ",") {
				t.Errorf("parse(%q) = %v, want %v", tc.args, lines, tc.want)
			}
		})
	}
}

// TestAnAmbiguousOptionSaysWhatItCouldHaveBeen.
//
// The error is what somebody reads when their abbreviation stopped working
// because a new option was added next to it. "Ambiguous" alone leaves them
// guessing; the list tells them what to type instead.
func TestAnAmbiguousOptionSaysWhatItCouldHaveBeen(t *testing.T) {
	t.Parallel()

	_, err := parse([]string{"--dr"}, []*option{tr("dry!", "--dry-run"), ig("dry1!", "x")})
	if err == nil {
		t.Fatal("--dr was accepted")
	}
	for _, want := range []string{"--dry", "--dry1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %s", err, want)
		}
	}
}

func translated(t *testing.T, argv ...string) *Plan {
	t.Helper()

	p, err := Translate(argv)
	if err != nil {
		t.Fatalf("Translate(%q) error = %v", argv, err)
	}
	return p
}

// joined renders a plan's arguments for comparison and for failure messages.
func joined(p *Plan) string { return strings.Join(p.Args, " ") }

// TestAWorkingImapsyncLineBecomesAWorkingOne.
//
// The command line is the one this project was started to replace, near enough:
// two hosts, two users, two passwords, a folder, and the two flags that were
// needed to stop it duplicating mail.
func TestAWorkingImapsyncLineBecomesAWorkingOne(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "imap.mail.me.com", "--user1", "apple@example.test", "--password1", "hunter2", "--ssl1",
		"--host2", "mox.example.net", "--user2", "jens@example.test", "--password2", "s3cret", "--ssl2",
		"--folder", "Archive", "--useuid", "--addheader", "--delete2")

	want := "sync " +
		"--source-url imaps://apple%40example.test@imap.mail.me.com:993 --source-password-env IMAPSYNC_COMPAT_PASSWORD1 " +
		"--dest-url imaps://jens%40example.test@mox.example.net:993 --dest-password-env IMAPSYNC_COMPAT_PASSWORD2 " +
		"--folder Archive --delete2"
	if got := joined(p); got != want {
		t.Errorf("translated to:\n  %s\nwant:\n  %s", got, want)
	}

	if p.Env["IMAPSYNC_COMPAT_PASSWORD1"] != "hunter2" || p.Env["IMAPSYNC_COMPAT_PASSWORD2"] != "s3cret" {
		t.Errorf("passwords did not reach the environment: %v", p.Env)
	}
	if len(p.Ignored) != 2 {
		t.Errorf("expected --useuid and --addheader to be reported as doing nothing, got %v", p.Ignored)
	}
}

// TestAPasswordNeverBecomesAnArgument.
//
// Arguments are readable by every process on the machine, and a translated
// command line is exactly the sort of thing that gets pasted into a bug report.
// imapsync takes the password on the command line; that is not a reason to
// carry the habit forward when the translation is being written out anyway.
func TestAPasswordNeverBecomesAnArgument(t *testing.T) {
	t.Parallel()

	const secret = "correct-horse-battery-staple"
	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", secret, "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", secret, "--ssl2")

	if strings.Contains(joined(p), secret) {
		t.Errorf("the password is in the arguments: %s", joined(p))
	}
	if strings.Contains(p.Explain("imapsync-go"), secret) {
		t.Errorf("the password is in the printed translation")
	}
}

// TestEveryRefusalIsReportedAtOnce.
//
// Somebody porting a command line with four unsupported flags should find that
// out in one run rather than in four.
func TestEveryRefusalIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	_, err := Translate([]string{
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--truncmess", "1000", "--regextrans2", "s/a/b/", "--delete1", "--synclabels",
	})

	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Translate() error = %v, want a RefusedError", err)
	}
	if len(refused.Refusals) != 4 {
		t.Fatalf("reported %d refusals, want 4: %v", len(refused.Refusals), refused.Refusals)
	}
	for _, want := range []string{"--truncmess", "--regextrans2", "--delete1", "--synclabels"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s:\n%s", want, err)
		}
	}
}

// TestARefusalSaysWhatToDoInstead.
//
// A refusal that only says no leaves somebody stuck. Every one of these has a
// perfectly good native answer and the message has to carry it, because the
// person reading it has a working imapsync command and no reason to go looking
// through this tool's help for the equivalent.
func TestARefusalSaysWhatToDoInstead(t *testing.T) {
	t.Parallel()

	tests := []struct{ option, wants string }{
		{"--folderrec=Parent", "--include"},
		{"--regextrans2=s/a/b/", "--map"},
		{"--maxbytespersecond=1000", "--dest-connections"},
		{"--justconnect", "imapsync-go probe"},
		{"--gmail1", "--host1"},
		{"--version", "--version"},
	}

	for _, tc := range tests {
		t.Run(tc.option, func(t *testing.T) {
			t.Parallel()

			_, err := Translate([]string{tc.option})
			if err == nil {
				t.Fatalf("%s was accepted", tc.option)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusing %s does not mention %s:\n%s", tc.option, tc.wants, err)
			}
		})
	}
}

// TestAnUnknownOptionIsRefused.
//
// The alternative is to pass it through, which means a typo silently syncs
// something other than what was asked for.
func TestAnUnknownOptionIsRefused(t *testing.T) {
	t.Parallel()

	_, err := Translate([]string{"--host1", "a", "--user1", "u", "--password1", "p", "--nosuchthing"})
	if err == nil {
		t.Fatal("--nosuchthing was accepted")
	}
	if !strings.Contains(err.Error(), "unknown option --nosuchthing") {
		t.Errorf("error = %v, want it to name the unknown option", err)
	}
}

// TestGuessingAtTLSIsSaidOutLoud.
//
// imapsync probes port 993 when told neither --ssl1 nor --tls1. Doing the same
// would make a translation depend on what a server happened to be doing at the
// time. Assuming TLS gets the same answer for any server on the internet, and
// the assumption is printed because the alternative to being right is sending
// a password in the clear.
func TestGuessingAtTLSIsSaidOutLoud(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p")

	if !strings.Contains(joined(p), "imaps://u@a.example.test:993") {
		t.Errorf("did not assume TLS: %s", joined(p))
	}
	if len(p.Assumed) != 2 {
		t.Errorf("made %d assumptions, want one per side: %v", len(p.Assumed), p.Assumed)
	}
	if !strings.Contains(p.Explain("x"), "assumed:") {
		t.Errorf("the assumption is not printed:\n%s", p.Explain("x"))
	}
}

// TestBeingToldHowToConnectEndsTheGuessing.
func TestBeingToldHowToConnectEndsTheGuessing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		flags  []string
		scheme string
	}{
		{name: "ssl", flags: []string{"--ssl1", "--ssl2"}, scheme: "imaps://u@a.example.test:993"},
		{name: "starttls", flags: []string{"--tls1", "--tls2"}, scheme: "imap://u@a.example.test:143"},
		{name: "neither, said explicitly", flags: []string{"--nossl1", "--nossl2"}, scheme: "imap+insecure://u@a.example.test:143"},
		{name: "no starttls either", flags: []string{"--notls1", "--notls2"}, scheme: "imap+insecure://u@a.example.test:143"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string{
				"--host1", "a.example.test", "--user1", "u", "--password1", "p",
				"--host2", "b.example.test", "--user2", "u", "--password2", "p",
			}, tc.flags...)

			p := translated(t, args...)
			if !strings.Contains(joined(p), tc.scheme) {
				t.Errorf("got %s, want it to contain %s", joined(p), tc.scheme)
			}
			if len(p.Assumed) != 0 {
				t.Errorf("assumed something despite being told: %v", p.Assumed)
			}
		})
	}
}

// TestAPortSurvivesTheScheme.
func TestAPortSurvivesTheScheme(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1", "--port1", "9993",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--tls2")

	if !strings.Contains(joined(p), "imaps://u@a.example.test:9993") {
		t.Errorf("port1 was lost: %s", joined(p))
	}
	if !strings.Contains(joined(p), "imap://u@b.example.test:143") {
		t.Errorf("the default STARTTLS port is wrong: %s", joined(p))
	}
}

// TestOffice365IsExactlyWhatImapsyncSaysItIs.
//
// imapsync prints "--office1 is like: --host1 outlook.office365.com --ssl1
// --exclude ^Files$" and then does that. All three have equivalents here, so
// this is one of the few bundles that can be honoured rather than refused.
func TestOffice365IsExactlyWhatImapsyncSaysItIs(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--office1", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2")

	got := joined(p)
	for _, want := range []string{"imaps://u@outlook.office365.com:993", "--exclude ^Files$"} {
		if !strings.Contains(got, want) {
			t.Errorf("--office1 did not produce %s: %s", want, got)
		}
	}
}

// TestAHostGivenAfterOffice365Wins.
//
// imapsync writes the host with ||=, so an explicit --host1 beats the bundle
// whichever order they appear in.
func TestAHostGivenAfterOffice365Wins(t *testing.T) {
	t.Parallel()

	for _, order := range [][]string{
		{"--office1", "--host1", "mine.example.test"},
		{"--host1", "mine.example.test", "--office1"},
	} {
		args := append(append([]string{}, order...),
			"--user1", "u", "--password1", "p",
			"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2")

		p := translated(t, args...)
		if !strings.Contains(joined(p), "mine.example.test") {
			t.Errorf("with %v the explicit host was lost: %s", order, joined(p))
		}
	}
}

// TestNegationTurnsTheNativeFlagOff.
func TestNegationTurnsTheNativeFlagOff(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--noautomap", "--noresyncflags", "--nosubscribe")

	for _, want := range []string{"--automap=false", "--resyncflags=false", "--subscribe=false"} {
		if !strings.Contains(joined(p), want) {
			t.Errorf("missing %s in %s", want, joined(p))
		}
	}
}

// TestTheSameNativeFlagIsNotEmittedTwice.
//
// Every --debug variant asks for debug logging. Repeating it would do no harm
// but would make the echoed command line look like nobody was paying attention,
// and the echoed command line is the thing people are meant to copy.
func TestTheSameNativeFlagIsNotEmittedTwice(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--debug", "--debuglist", "--debugflags")

	if n := strings.Count(joined(p), "--log-level"); n != 1 {
		t.Errorf("--log-level appears %d times in %s", n, joined(p))
	}
}

// TestTimeoutsAreConvertedRatherThanCopied.
//
// imapsync counts seconds; this tool takes a duration. A bare "30" is not one,
// and passing it through would fail at the far end with a message about
// something the user never typed.
func TestTimeoutsAreConvertedRatherThanCopied(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--timeout", "45")

	if !strings.Contains(joined(p), "--dial-timeout 45s") {
		t.Errorf("timeout was not converted: %s", joined(p))
	}
}

// TestAnEndpointMissingItsPartsSaysWhichPart.
func TestAnEndpointMissingItsPartsSaysWhichPart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		wants string
	}{
		{name: "no host", args: []string{"--user1", "u"}, wants: "--host1"},
		{name: "no user", args: []string{"--host1", "a"}, wants: "--user1"},
		{name: "no password", args: []string{"--host1", "a", "--user1", "u"}, wants: "--password1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Translate(tc.args)
			if err == nil {
				t.Fatalf("Translate(%q) succeeded", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v, want it to name %s", err, tc.wants)
			}
		})
	}
}

// TestAPasswordFileIsPassedThroughAsAFile.
//
// The better half of imapsync's two ways of giving a password, and it needs no
// environment variable at all.
func TestAPasswordFileIsPassedThroughAsAFile(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--passfile1", "/etc/secret1", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--passfile2", "/etc/secret2", "--ssl2")

	for _, want := range []string{"--source-password-file /etc/secret1", "--dest-password-file /etc/secret2"} {
		if !strings.Contains(joined(p), want) {
			t.Errorf("missing %s in %s", want, joined(p))
		}
	}
	if len(p.Env) != 0 {
		t.Errorf("a password file should need no environment variable, got %v", p.Env)
	}
}

// TestTurningOffTheTLSProbeMeansSkippingVerification.
//
// --nosslcheck is imapsync's way of saying "stop poking at my server", and the
// only part of it that reaches this tool is the certificate check.
func TestTurningOffTheTLSProbeMeansSkippingVerification(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--nosslcheck")

	for _, want := range []string{"--source-insecure", "--dest-insecure"} {
		if !strings.Contains(joined(p), want) {
			t.Errorf("missing %s in %s", want, joined(p))
		}
	}
}

// TestAnOptionThatDidNothingIsStillReported.
//
// The whole difference between this and ignoring a flag is that this says so.
func TestAnOptionThatDidNothingIsStillReported(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--useuid", "--tmpdir", "/tmp", "--pidfile", "/run/x.pid")

	text := p.Explain("imapsync-go")
	for _, want := range []string{"--useuid", "--tmpdir", "--pidfile", "accepted but did nothing"} {
		if !strings.Contains(text, want) {
			t.Errorf("the explanation does not mention %s:\n%s", want, text)
		}
	}
}

// TestTheExplanationCanBePastedBackIntoAShell.
func TestTheExplanationCanBePastedBackIntoAShell(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p", "--ssl1",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p", "--ssl2",
		"--folder", "Sent Items")

	if !strings.Contains(p.Explain("imapsync-go"), "'Sent Items'") {
		t.Errorf("a folder with a space was not quoted:\n%s", p.Explain("imapsync-go"))
	}
}

// TestTheSelectionOptionsCarryTheirUnitsAcross.
//
// imapsync takes sizes as bare bytes and ages as bare days. This tool's own
// flags take human-readable values, which is the house style everywhere else —
// --memory-limit "256MiB" — so the numbers have to be given their units on the
// way through. A bare "30" reaching --max-age would be refused as unparseable,
// and a bare "30" reaching --max-size would silently mean thirty bytes.
func TestTheSelectionOptionsCarryTheirUnitsAcross(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--maxsize", "10485760", "--minsize", "1024",
		"--maxage", "30", "--minage", "7",
	)

	for _, want := range []string{
		"--max-size 10485760", "--min-size 1024",
		"--max-age 30d", "--min-age 7d",
	} {
		if !strings.Contains(joined(p), want) {
			t.Errorf("translated line does not contain %q:\n  %s", want, joined(p))
		}
	}
}

// imapsync declares --maxage and --minage as floats, so a fraction of a day is
// a command line somebody can legitimately type.
func TestAFractionalAgeSurvivesTranslation(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--maxage", "0.5",
	)
	if !strings.Contains(joined(p), "--max-age 0.5d") {
		t.Errorf("a half-day age did not survive:\n  %s", joined(p))
	}
}

// TestStatingAnAppendLimitByHandIsAcceptedAndExplained.
//
// The reason this is worth a test is that the explanation used to be false. It
// said "the server's APPENDLIMIT is obeyed as reported" while nothing read the
// capability at all, so an oversized message was fetched, appended, refused and
// filed as a failure. The capability is obeyed now, and the note has to say
// what is actually true and what to reach for instead.
func TestStatingAnAppendLimitByHandIsAcceptedAndExplained(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--appendlimit", "10485760",
	)

	var why string
	for _, note := range p.Ignored {
		if note.Option == "--appendlimit" {
			why = note.Why
		}
	}
	if why == "" {
		t.Fatalf("--appendlimit was not reported as ignored: %v", p.Ignored)
	}
	if !strings.Contains(why, "--max-size") {
		t.Errorf("the note does not say what to use instead: %q", why)
	}
}

// TestNoabletosearchSelectsTheInternalDateBasis.
//
// imapsync measures --maxage and --minage from the Date: header, and
// --noabletosearch switches them to the internal date. This tool reads the
// Date: header out of headers it already fetches, so it never has to ask
// whether the server can search — but the choice of date is real, and dropping
// --noabletosearch on the floor would measure ages from the wrong one while
// reporting success.
func TestNoabletosearchSelectsTheInternalDateBasis(t *testing.T) {
	t.Parallel()

	for _, option := range []string{"--noabletosearch", "--noabletosearch1"} {
		p := translated(t,
			"--host1", "a.example.test", "--user1", "u", "--password1", "p",
			"--host2", "b.example.test", "--user2", "u", "--password2", "p",
			"--maxage", "30", option,
		)
		if !strings.Contains(joined(p), "--age-basis internal") {
			t.Errorf("%s did not select the internal basis:\n  %s", option, joined(p))
		}
	}
}

// The positive spelling asks for what already happens, so it is ignored — but
// the note has to say so truthfully. It used to read "this is already what
// happens" while ages were in fact measured from the internal date, which was
// the exact opposite of what --abletosearch asks for.
func TestAbletosearchIsIgnoredBecauseItIsTheDefault(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--abletosearch",
	)
	if strings.Contains(joined(p), "--age-basis") {
		t.Errorf("--abletosearch emitted a flag; it is the default:\n  %s", joined(p))
	}

	var why string
	for _, note := range p.Ignored {
		if note.Option == "--abletosearch" {
			why = note.Why
		}
	}
	if !strings.Contains(why, "Date:") {
		t.Errorf("the note does not name the basis it claims is already in use: %q", why)
	}
}

// --noabletosearch2 asks for a destination-side age basis, and this tool never
// selects destination messages by age. Saying so is better than translating it
// into a source-side change the user did not ask for.
func TestNoabletosearch2IsIgnoredBecauseTheDestinationIsNotAgeFiltered(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--noabletosearch2",
	)
	if strings.Contains(joined(p), "--age-basis") {
		t.Errorf("--noabletosearch2 changed the source basis:\n  %s", joined(p))
	}

	var found bool
	for _, note := range p.Ignored {
		if note.Option == "--noabletosearch2" {
			found = true
			if !strings.Contains(note.Why, "destination") {
				t.Errorf("the note does not explain which side is unaffected: %q", note.Why)
			}
		}
	}
	if !found {
		t.Errorf("--noabletosearch2 was not reported as ignored: %v", p.Ignored)
	}
}

// TestSearchGoesToBothSidesAndTheHalvesGoToOne.
//
// imapsync spells one option where this tool spells two, so --search has to
// arrive as a value on each of them rather than as a value after the last of
// them. Getting that wrong is not a visible failure: "--source-search
// --dest-search UNSEEN" parses, and leaves the source unfiltered while
// silently swallowing the next flag as --source-search's value.
func TestSearchGoesToBothSidesAndTheHalvesGoToOne(t *testing.T) {
	t.Parallel()

	base := []string{
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
	}

	both := joined(translated(t, append(base, "--search", "UNSEEN")...))
	if !strings.Contains(both, "--source-search UNSEEN") || !strings.Contains(both, "--dest-search UNSEEN") {
		t.Errorf("--search did not reach both sides:\n  %s", both)
	}

	one := joined(translated(t, append(base, "--search1", "SEEN")...))
	if !strings.Contains(one, "--source-search SEEN") {
		t.Errorf("--search1 did not reach the source:\n  %s", one)
	}
	if strings.Contains(one, "--dest-search") {
		t.Errorf("--search1 reached the destination as well:\n  %s", one)
	}

	two := joined(translated(t, append(base, "--search2", "DELETED")...))
	if !strings.Contains(two, "--dest-search DELETED") {
		t.Errorf("--search2 did not reach the destination:\n  %s", two)
	}
	if strings.Contains(two, "--source-search") {
		t.Errorf("--search2 reached the source as well:\n  %s", two)
	}
}

// A search key with spaces in it is one argument, and has to stay one across
// the translation. This is the case that catches a translator which joins its
// output by spaces and hands the result to a shell.
func TestASearchKeyWithSpacesStaysOneArgument(t *testing.T) {
	t.Parallel()

	p := translated(t,
		"--host1", "a.example.test", "--user1", "u", "--password1", "p",
		"--host2", "b.example.test", "--user2", "u", "--password2", "p",
		"--search1", "UNSEEN SMALLER 1000",
	)

	var found bool
	for i, arg := range p.Args {
		if arg != "--source-search" {
			continue
		}
		if i+1 >= len(p.Args) || p.Args[i+1] != "UNSEEN SMALLER 1000" {
			t.Fatalf("--source-search was not followed by the whole key: %q", p.Args)
		}
		found = true
	}
	if !found {
		t.Fatalf("--source-search is absent from %q", p.Args)
	}
}
