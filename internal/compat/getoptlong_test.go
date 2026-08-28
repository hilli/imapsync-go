package compat

import (
	"strings"
	"testing"
)

// perlSays records what Getopt::Long actually did with each of these command
// lines, given imapsync's own option declarations.
//
// It was produced by running a Perl script that declares all 218 options and
// prints what it resolved each spelling to. That is the only way to be sure
// about abbreviation: whether --auto is ambiguous depends on every other
// option in the table, so it cannot be reasoned about one option at a time, and
// the answer changes as the table does.
//
// A want of "ambiguous" or "unknown" means Getopt::Long refused; anything else
// is the option it resolved to and the value it gave, written the way Perl
// wrote it.
var perlSays = []struct{ args, want string }{
	{"--fold INBOX", "ambiguous"},
	{"--folder INBOX", "folder=INBOX"},
	{"--folderr X", "folderrec=X"},
	{"--dry", "dry=1"},
	{"--nodry", "dry=0"},
	{"--no-dry", "dry=0"},
	{"--dry1", "dry1=1"},
	{"--del", "ambiguous"},
	{"--delete2", "delete2=1"},
	{"--delete", "ambiguous"},
	{"--nodelete2", "delete2=0"},
	{"--exp", "ambiguous"},
	{"--expunge", "expunge1=1"},
	{"--expunge1", "expunge1=1"},
	{"--expung", "ambiguous"},
	{"--host1 h", "host1=h"},
	{"-host1 h", "host1=h"},
	{"--host1=h", "host1=h"},
	{"--HOST1 h", "host1=h"},
	{"--ssl", "ambiguous"},
	{"--ssl1", "ssl1=1"},
	{"--nossl1", "ssl1=0"},
	{"--tls1", "tls1=1"},
	{"--automap", "automap=1"},
	{"--noautomap", "automap=0"},
	{"--auto", "automap=1"},
	{"--useuid", "useuid=1"},
	{"--addheader", "addheader=1"},
	{"--subscribe", "subscribe=1"},
	{"--nosubscribe", "subscribe=0"},
	{"--subscribeall", "subscribeall=1"},
	{"--subscribed", "subscribed=1"},
	{"--timeout 30", "timeout=30"},
	{"--max", "ambiguous"},
	{"--maxage 30", "maxage=30"},
	{"--maxs", "ambiguous"},
	{"--debug", "debug=1"},
	{"--debugimap", "debugimap=1"},
	{"--debu", "ambiguous"},
	{"--sslcheck", "sslcheck=1"},
	{"--nosslcheck", "sslcheck=0"},
	{"--office1", "office1=1"},
	{"--gmail1", "gmail1=1"},
	{"--f1f2 A=B", "f1f2=A=B"},
	{"--nof1f2", "nof1f2=1"},
	{"--exclude X", "exclude=X"},
	{"--noexclude", "noexclude=1"},
	{"--include X", "include=X"},
	{"--regextrans2 s/a/b/", "regextrans2=s/a/b/"},
	{"--justconnect", "justconnect=1"},
	{"--version", "version=1"},
	{"--help", "help=1"},
	{"--tmpdir /tmp", "tmpdir=/tmp"},
	{"--pidfile /x", "pidfile=/x"},
	{"--wibble", "unknown"},
}

// TestThisAgreesWithGetoptLong.
//
// The drop-in promise is that a command line which worked yesterday works
// today, and abbreviation is where that promise is easiest to break by
// accident: --auto resolves because only one option begins that way, while
// --expung is ambiguous between four, and both facts are properties of the
// whole table rather than of any option in it.
func TestThisAgreesWithGetoptLong(t *testing.T) {
	t.Parallel()

	opts := table()
	for _, tc := range perlSays {
		t.Run(tc.args, func(t *testing.T) {
			t.Parallel()

			got, err := parse(strings.Fields(tc.args), opts)
			switch tc.want {
			case "ambiguous", "unknown":
				if err == nil {
					t.Fatalf("parse(%q) succeeded; Getopt::Long calls it %s", tc.args, tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("parse(%q) error = %v, want %s", tc.args, err, tc.want)
				}
				return
			}

			if err != nil {
				t.Fatalf("parse(%q) error = %v, want %s", tc.args, err, tc.want)
			}
			if len(got) != 1 {
				t.Fatalf("parse(%q) returned %d options, want 1", tc.args, len(got))
			}

			g := got[0]
			value := "1"
			switch {
			case g.opt.kind.takesValue():
				value = g.value
			case !g.on:
				value = "0"
			}
			if line := g.opt.name() + "=" + value; line != tc.want {
				t.Errorf("parse(%q) = %s, Getopt::Long says %s", tc.args, line, tc.want)
			}
		})
	}
}
