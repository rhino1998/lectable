package patreon_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/patreon/patreontest"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

func resolve(t *testing.T, src websource.Source, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Match(u) {
		t.Fatalf("%s isn't matched", link)
	}
	ids, err := src.Resolve(context.Background(), src.Fetch(), u)
	if err != nil || len(ids) != 1 {
		t.Fatalf("Resolve(%s) = %v, %v", link, ids, err)
	}
	return ids[0]
}

func titles(work *websource.Work) string {
	var out []string
	for _, ref := range work.Chapters {
		title := ref.Title
		if !ref.Unlocked {
			title += " (locked)"
		}
		out = append(out, title)
	}
	return strings.Join(out, "|")
}

func TestPatreon(t *testing.T) {
	site := patreontest.New(t)
	src := site.Source()
	ctx := context.Background()
	welcome := site.AddPublic("Welcome", `<p>Hello &amp; thanks.</p><script>track()</script><p>More soon.</p>`)
	site.AddPoll("What next?")
	one := site.AddPatronsOnly("Chapter 1: Setting Out", "The road was long.", "“Are we there yet?” Sam asked.")
	site.AddPatronsOnly("Chapter 2: The Inn", "The inn was warm.")
	site.AddPublic("Schedule update", `<p>Back on Monday.</p>`)

	// A creator's page in each of its spellings, one of their posts, and a
	// collection.
	for _, link := range []string{site.Link(), "https://www.patreon.com/c/" + patreontest.Vanity + "/posts", "https://patreon.com/cw/" + patreontest.Vanity, "https://www.patreon.com/posts/chapter-1-setting-out-" + one} {
		if id := resolve(t, src, link); id != patreontest.CampaignID {
			t.Errorf("Resolve(%s) = %q", link, id)
		}
	}
	collection := resolve(t, src, "https://www.patreon.com/collection/"+patreontest.CollectionID)
	if collection != patreontest.CampaignID+"/"+patreontest.CollectionID {
		t.Errorf("collection resolved to %q", collection)
	}
	for _, link := range []string{"https://www.patreon.com/nobody", "https://www.patreon.com/login", "https://www.patreon.com/user?u=5"} {
		u, _ := url.Parse(link)
		if ids, err := src.Resolve(ctx, src.Fetch(), u); err == nil {
			t.Errorf("Resolve(%s) = %v, want an error", link, ids)
		}
	}

	// A stranger sees every post listed (over several pages), the
	// patrons-only ones locked, and no polls.
	work, err := src.Work(ctx, src.Fetch(), patreontest.CampaignID)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(work); got != "Welcome|Chapter 1: Setting Out (locked)|Chapter 2: The Inn (locked)|Schedule update" {
		t.Errorf("a stranger's list = %q", got)
	}
	if work.Title != "The Long Road" || work.Author != "A. Writer" || work.URL != site.Link() {
		t.Errorf("work = %q by %q at %s", work.Title, work.Author, work.URL)
	}
	ch, err := src.Chapter(ctx, src.Fetch(), work.Chapters[0])
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != welcome || ch.Body != `<p>Hello &amp; thanks.</p><p>More soon.</p>` {
		t.Errorf("public post = %q %q", ch.ID, ch.Body)
	}
	if _, err := src.Chapter(ctx, src.Fetch(), work.Chapters[1]); !errors.Is(err, websource.ErrLocked) {
		t.Errorf("a locked post read as %v", err)
	}
	// Even one the list said was open, if the post itself says otherwise.
	open := work.Chapters[1]
	open.Unlocked = true
	if _, err := src.Chapter(ctx, src.Fetch(), open); !errors.Is(err, websource.ErrLocked) {
		t.Errorf("a locked post asked for anyway read as %v", err)
	}
	if name, err := src.Account(ctx); err != nil || name != "" {
		t.Errorf("a stranger's account = %q, %v", name, err)
	}

	// A member reads them.
	src.SetSession(patreontest.MemberSession)
	if name, err := src.Account(ctx); err != nil || name != "Sam Reader" {
		t.Errorf("account = %q, %v", name, err)
	}
	work, err = src.Work(ctx, src.Fetch(), collection)
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(work); got != "Chapter 1: Setting Out|Chapter 2: The Inn" || work.Title != "The Long Road: Advance Chapters" {
		t.Errorf("the collection = %q: %q", work.Title, got)
	}
	ch, err = src.Chapter(ctx, src.Fetch(), work.Chapters[0])
	if err != nil {
		t.Fatal(err)
	}
	if ch.Body != `<p>The road was long.</p><p>“Are we there yet?” Sam asked.</p>` {
		t.Errorf("patrons-only post = %q", ch.Body)
	}
}
