package syncer_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// subscribedTo reports whether the destination lists a mailbox as subscribed.
func subscribedTo(t *testing.T, a account, mailbox string) bool {
	t.Helper()

	conn := a.dial(t)
	folders, err := conn.ListFolders(context.Background(), imapx.ListOptions{})
	if err != nil {
		t.Fatalf("listing folders: %v", err)
	}
	for _, f := range folders {
		if f.Name == mailbox {
			return slices.Contains(f.Attrs, string(imap.MailboxAttrSubscribed))
		}
	}
	t.Fatalf("destination has no mailbox %q", mailbox)
	return false
}

// TestACreatedFolderIsSubscribed.
//
// A mailbox that exists but is not subscribed does not appear in any client
// that browses by LSUB, which is the default in most of them. The mail is
// there, the folder is not, and from the owner's side that is indistinguishable
// from the sync having skipped it. imapsync subscribes what it creates for this
// reason and so does this.
func TestACreatedFolderIsSubscribed(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps(), "Work")
	h.src.stuff(t, "Work", testMessage("a report", "sub-1@example.test"))

	report := h.run(t)
	if len(report.Created) == 0 {
		t.Fatalf("nothing was created, so nothing could be subscribed")
	}

	if !subscribedTo(t, h.dst, "Work") {
		t.Errorf("created folder Work is not subscribed, so it will not show up in a mail client")
	}
}

// TestSubscribingCanBeDeclined.
//
// Somebody synchronising into a destination they curate by hand may not want
// every copied folder appearing in their client. The flag is negative because
// the default has to be the imapsync one.
func TestSubscribingCanBeDeclined(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps(), "Work")
	h.src.stuff(t, "Work", testMessage("a report", "sub-2@example.test"))

	h.run(t, func(o *syncer.Options) { o.NoSubscribe = true })

	if subscribedTo(t, h.dst, "Work") {
		t.Errorf("Work was subscribed despite NoSubscribe")
	}
}

type cannotSubscribe struct{ imapx.Conn }

func (c cannotSubscribe) SubscribeFolder(context.Context, string) error {
	return errors.New("SUBSCRIBE not supported")
}

// TestMailStillArrivesWhenSubscribingIsRefused.
//
// Not every server implements SUBSCRIBE, and the ones that do may refuse for
// reasons of their own. Refusing to subscribe is a cosmetic loss; abandoning
// the copy over it would turn a folder that is merely hard to find into one
// that is empty. The copy has to win.
func TestMailStillArrivesWhenSubscribingIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps(), "Work")
	h.src.stuff(t, "Work", testMessage("a report", "sub-3@example.test"))

	s := syncer.New(
		pooled(t, 2, readOnly, h.src.dialFunc(t, nil)),
		pooled(t, 2, imapx.SelectOptions{}, h.dst.dialFunc(t, func(c imapx.Conn) imapx.Conn {
			return cannotSubscribe{c}
		})),
		h.db, nil, syncer.Options{PairID: "test"},
	)
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var copied int
	for _, fr := range report.Folders {
		copied += fr.Copied
	}
	if copied != 1 {
		t.Errorf("copied %d messages, want 1: a refused subscription stopped the copy", copied)
	}
	if got := subjects(h.dst.contents(t, "Work")); len(got) != 1 {
		t.Errorf("destination Work holds %v, want the one message", got)
	}
}

// TestPoolConnectionsDoNotSubscribeWhatTheyDidNotCreate.
//
// Subscribing is scoped to folders this run brought into existence. A
// destination folder the owner deliberately unsubscribed is theirs to leave
// that way, and a sync that re-subscribed it on every run would be an argument
// they could not win.
func TestPoolConnectionsDoNotSubscribeWhatTheyDidNotCreate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev2Caps(), "Work")
	h.src.stuff(t, "Work", testMessage("a report", "sub-4@example.test"))

	if err := h.dst.user.Create("Work", nil); err != nil {
		t.Fatalf("pre-creating Work: %v", err)
	}
	if err := h.dst.user.Unsubscribe("Work"); err != nil {
		t.Fatalf("unsubscribing Work: %v", err)
	}

	h.run(t)

	if subscribedTo(t, h.dst, "Work") {
		t.Errorf("a folder the owner unsubscribed was subscribed again by the sync")
	}
}
