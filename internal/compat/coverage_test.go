package compat

import (
	"sort"
	"strings"
	"testing"
)

// imapsyncOptions is every option imapsync declares, taken from the two
// Getopt::Long calls in imapsync 2.x. Where both calls declare the same option
// the one naming every alias is kept, which is why expunge1 appears here with
// its alias and not without.
//
// It is here so the table can be checked against the thing it imitates rather
// than against somebody's memory of it. When imapsync gains an option the two
// go out of step and the test says which one is missing, which matters because
// an option missing from the table is refused as unknown — and being refused
// for having been forgotten looks exactly like being refused on purpose.
// These are imapsync's own spellings, copied verbatim; they are option names
// rather than prose, so the spell checker has no business in here.
//
//nolint:misspell // renaming an option would stop it working.
var imapsyncOptions = []string{
	"abort",
	"abortbyfile",
	"host1=s",
	"host2=s",
	"user1=s",
	"user2=s",
	"password1=s",
	"password2=s",
	"dry!",
	"dry1!",
	"version",
	"ssl1!",
	"ssl2!",
	"tls1!",
	"tls2!",
	"compress1!",
	"compress2!",
	"justbanner!",
	"justlogin!",
	"justconnect!",
	"addheader!",
	"automap!",
	"justautomap!",
	"gmail1",
	"gmail2",
	"office1",
	"office2",
	"exchange1",
	"exchange2",
	"domino1",
	"domino2",
	"f1f2=s@",
	"f1f2h=s%",
	"folder=s@",
	"folderrec=s@",
	"testslive!",
	"testslive2!",
	"testslive6!",
	"releasecheck!",
	"simulong=f",
	"debugsleep=f",
	"subfolder1=s",
	"subfolder2=s",
	"justfolders!",
	"justfoldersizes!",
	"delete1!",
	"expunge1|expunge!",
	"delete2!",
	"expunge2!",
	"uidexpunge2!",
	"delete2duplicates!",
	"delete1emptyfolders",
	"delete2folders!",
	"tail!",
	"exitwhenover=i",
	"exitonload!",
	"syncduplicates!",
	"skipcrossduplicates!",
	"debugcrossduplicates!",
	"log!",
	"loglogfile!",
	"emailreportfrom=s",
	"emailreport1!",
	"emailreport2!",
	"var=s@",
	"extra=s@",
	"useheader=s@",
	"minsize=i",
	"maxsize=i",
	"maxage=f",
	"minage=f",
	"search=s",
	"search1=s",
	"search2=s",
	"debug!",
	"debuglist!",
	"debugcontent!",
	"debugflags!",
	"debugimap!",
	"debugimap1!",
	"debugimap2!",
	"debugmemory!",
	"debugfolders!",
	"debugssl=i",
	"debugenv!",
	"debugsig!",
	"debuglabels!",
	"port1=i",
	"port2=i",
	"inet4|ipv4",
	"inet6|ipv6",
	"domain1=s",
	"domain2=s",
	"passfile1=s",
	"passfile2=s",
	"authmd5!",
	"authmd51!",
	"authmd52!",
	"trylogin!",
	"oauthdirect1=s",
	"oauthdirect2=s",
	"oauthaccesstoken1=s",
	"oauthaccesstoken2=s",
	"oauthrefreshcmd1=s",
	"oauthrefreshcmd2=s",
	"sep1=s",
	"sep2=s",
	"sanitize!",
	"include=s",
	"exclude=s",
	"noexclude",
	"folderfirst=s",
	"folderlast=s",
	"prefix1=s",
	"prefix2=s",
	"fixslash2!",
	"fixInboxINBOX!",
	"regextrans2=s@",
	"mixfolders!",
	"skipemptyfolders!",
	"regexmess=s",
	"noregexmess",
	"skipmess=s",
	"pipemess=s",
	"pipemesscheck!",
	"disarmreadreceipts!",
	"regexflag=s@",
	"noregexflag",
	"filterflags!",
	"filterbuggyflags!",
	"flagscase!",
	"syncflagsaftercopy!",
	"resyncflags!",
	"synclabels!",
	"resynclabels!",
	"delete2foldersonly=s",
	"delete2foldersbutnot=s",
	"syncinternaldates!",
	"idatefromheader!",
	"syncacls!",
	"appendlimit=i",
	"truncmess=i",
	"foldersizes!",
	"foldersizesatend!",
	"subscribed",
	"subscribe!",
	"subscribeall|subscribe_all!",
	"help",
	"timeout=f",
	"timeout1=f",
	"timeout2=f",
	"skipheader=s",
	"wholeheaderifneeded!",
	"messageidnodomain!",
	"skipsize!",
	"allowsizemismatch!",
	"fastio1!",
	"fastio2!",
	"sslcheck!",
	"ssl1_ssl_version=s",
	"ssl2_ssl_version=s",
	"sslargs1=s%",
	"sslargs2=s%",
	"uid1!",
	"uid2!",
	"authmech1=s",
	"authmech2=s",
	"authuser1=s",
	"authuser2=s",
	"proxyauth1",
	"proxyauth2",
	"keepalive1!",
	"keepalive2!",
	"split1=i",
	"split2=i",
	"buffersize=i",
	"reconnectretry1=i",
	"reconnectretry2=i",
	"tests!",
	"testsdebug!",
	"testsunit=s@",
	"tmpdir=s",
	"pidfile=s",
	"pidfilelocking!",
	"sigexit=s@",
	"sigreconnect=s@",
	"sigignore=s@",
	"modulesversion!",
	"usecache!",
	"cacheaftercopy!",
	"debugcache!",
	"useuid!",
	"checkselectable!",
	"checkfoldersexist!",
	"checkmessageexists!",
	"expungeaftereach!",
	"abletosearch!",
	"abletosearch1!",
	"abletosearch2!",
	"showpasswords!",
	"maxlinelength=i",
	"fixcolonbug!",
	"create_folder_old!",
	"maxmessagespersecond=f",
	"maxbytespersecond=i",
	"maxbytesafter=i",
	"maxsleep=f",
	"logfile=s",
	"logdir=s",
	"errorsmax=i",
	"errorsdump!",
	"fetch_hash_set=s",
	"id!",
	"nof1f2",
	"justfolderlists!",
	"checknoabletosearch!",
	"dockercontext!",
	"memorystress!",
}

// TestEveryImapsyncOptionIsAccountedFor.
//
// Not "accepted" — accounted for. Most of these are refused, and refusing is a
// perfectly good answer. What is not a good answer is a flag nobody looked at,
// because the person who typed it cannot tell that apart from a considered no.
func TestEveryImapsyncOptionIsAccountedFor(t *testing.T) {
	t.Parallel()

	have := make(map[string]*option)
	for _, o := range table() {
		for _, n := range o.names {
			if prev, dup := have[strings.ToLower(n)]; dup {
				t.Errorf("option %q is in the table twice, as %q and %q", n, prev.name(), o.name())
			}
			have[strings.ToLower(n)] = o
		}
	}

	var missing []string
	for _, spec := range imapsyncOptions {
		want := opt(spec, outcome{})
		got, ok := have[strings.ToLower(want.name())]
		if !ok {
			missing = append(missing, spec)
			continue
		}
		if got.kind != want.kind {
			t.Errorf("option %s: the table says it takes %v, imapsync declares %v", want.name(), got.kind, want.kind)
		}
		if strings.Join(got.names, "|") != strings.Join(want.names, "|") {
			t.Errorf("option %s: the table spells it %v, imapsync spells it %v", want.name(), got.names, want.names)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d imapsync options are missing from the table, so they would be refused as unknown:\n\t%s",
			len(missing), strings.Join(missing, "\n\t"))
	}
}

// TestTheTableInventsNothing.
//
// The other direction. An entry for an option imapsync does not have is either
// a typo, which leaves the real option unknown, or a flag somebody imagined,
// which nobody will ever type.
func TestTheTableInventsNothing(t *testing.T) {
	t.Parallel()

	known := make(map[string]bool)
	for _, spec := range imapsyncOptions {
		for _, n := range opt(spec, outcome{}).names {
			known[strings.ToLower(n)] = true
		}
	}

	for _, o := range table() {
		for _, n := range o.names {
			if !known[strings.ToLower(n)] {
				t.Errorf("the table has --%s, which imapsync does not", n)
			}
		}
	}
}
