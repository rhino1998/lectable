package wanderinginn_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/patreon/patreontest"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn/wanderinginntest"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// A chapter for patrons only is read by logging in to the site with the
// library's Patreon session, the way "Unlock with Patreon" does it.
func TestUnlockWithPatreon(t *testing.T) {
	pat := patreontest.New(t)
	site := wanderinginntest.New(t)
	site.Patreon, site.Tier = pat.Server.URL, 5
	open := site.AddChapter(1, "1-00", "1.00", `<p>The inn was dark.</p>`)
	locked := site.AddChapter(1, "1-01", "1.01", `<p>For patrons, for now.</p>`)
	higher := site.AddChapter(1, "1-02", "1.02", `<p>For bigger patrons.</p>`)
	site.SetLocked(locked, true)
	site.SetLocked(higher, true)
	site.SetTier(higher, 9)

	patreon := pat.Source()
	src := site.Source()
	src.Authorize = patreon.Authorize
	ctx := context.Background()
	read := func(id string) (string, error) {
		// Through a recorder, as an import reads: what is kept of a page
		// read twice must be the copy that had the text.
		rec := &websource.Recorder{Inner: src.Fetch()}
		ref := websource.ChapterRef{ID: id, Title: id, URL: site.Server.URL + "/" + id + "/", Unlocked: true}
		ch, err := src.Chapter(ctx, rec, ref)
		if err != nil {
			return "", err
		}
		again, err := src.Chapter(ctx, websource.Replay(rec.Fetches()), ref)
		if err != nil || again.Body != ch.Body {
			t.Errorf("%s read back from its stored pages: %v", id, err)
		}
		return ch.Body, nil
	}

	// Not signed in to Patreon: locked, as before.
	if _, err := read(locked); !errors.Is(err, websource.ErrLocked) {
		t.Fatalf("signed out, the locked chapter read as %v", err)
	}
	// Signed in: the site is logged in to on the way, and the chapter read.
	patreon.SetSession(patreontest.MemberSession)
	src.SignOut()
	body, err := read(locked)
	if err != nil || !strings.Contains(body, "For patrons, for now.") {
		t.Fatalf("signed in, the locked chapter = %q, %v", body, err)
	}
	// One the membership doesn't cover stays locked, and an open one is
	// read as ever.
	if _, err := read(higher); !errors.Is(err, websource.ErrLocked) {
		t.Errorf("a chapter above the membership read as %v", err)
	}
	if body, err := read(open); err != nil || !strings.Contains(body, "The inn was dark.") {
		t.Errorf("the open chapter = %q, %v", body, err)
	}
	// A consent link that would send the login somewhere else is refused.
	if _, err := patreon.Authorize(ctx, pat.Server.URL+"/oauth2/authorize?redirect_uri=https%3A%2F%2Felsewhere.test%2Fcb", "elsewhere.example"); err == nil {
		t.Error("a login sent to another host was allowed")
	}
}
