package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/compat"
)

// TestEveryTranslationNamesARealFlag.
//
// The compat table is written in terms of this program's flags, but it lives in
// a package that cannot see them. Nothing stops it naming a flag that does not
// exist, and nothing would notice: the translation would print, look right,
// and then fail at the far end complaining about a flag the user never typed
// and cannot find in any documentation.
func TestEveryTranslationNamesARealFlag(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	var sync *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "sync" {
			sync = c
		}
	}
	if sync == nil {
		t.Fatal("no sync command to translate into")
	}

	for _, name := range compat.NativeFlags() {
		if sync.Flags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("the compat table emits --%s, which sync does not accept", name)
		}
	}
}

// TestTheSymlinkIsWhatMakesThisADropIn.
//
// Installing this as "imapsync" is the whole point of the shim: existing cron
// lines and scripts name that binary and pass imapsync's flags, and they have
// no subcommand in them to hang the translation off.
func TestTheSymlinkIsWhatMakesThisADropIn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"invoked as imapsync", []string{"imapsync", "--host1", "a"}, []string{"compat", "--host1", "a"}},
		{"invoked by full path", []string{"/usr/local/bin/imapsync", "--dry"}, []string{"compat", "--dry"}},
		{"invoked by its own name", []string{"imapsync-go", "sync"}, []string{"sync"}},
		{"invoked with no arguments at all", []string{"imapsync"}, []string{"compat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := argsFor(tc.argv)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argsFor(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestCompatRefusesRatherThanGuessing checks the refusal reaches the user
// through the command, not just the translator: a shim that swallows its own
// objections and syncs anyway is worse than no shim.
func TestCompatRefusesRatherThanGuessing(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	root.SetArgs([]string{"compat", "--host1", "a", "--user1", "u", "--passfile1", "/dev/null",
		"--host2", "b", "--user2", "u", "--passfile2", "/dev/null", "--maxage", "30"})
	out := &strings.Builder{}
	root.SetOut(out)
	root.SetErr(out)

	err := root.Execute()
	if err == nil {
		t.Fatal("compat accepted --maxage, which changes which messages are copied")
	}
	if !strings.Contains(err.Error(), "maxage") {
		t.Errorf("error = %v, want it to name --maxage", err)
	}
}
