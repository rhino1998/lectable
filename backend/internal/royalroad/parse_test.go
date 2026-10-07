package royalroad

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// fictionPageHTML is a fiction page cut down to the parts parseFiction
// reads, laid out as the site does.
const fictionPageHTML = `<!DOCTYPE html><html><head>
<title>The Long Road | Royal Road</title>
<meta property="og:type" content="books.book">
<meta property="og:url" content="https://www.royalroad.com/fiction/4242/the-long-road">
<meta property="og:image" content="https://www.royalroadcdn.com/public/covers-large/4242-the-long-road.jpg?time=1">
<meta property="books:author" content="A. Writer"/>
<meta name="twitter:title" content="The Long Road">
<link rel="canonical" href="https://www.royalroad.com/fiction/4242/the-long-road"/>
<script type="application/ld+json">{"@context":"https://schema.org","@type":"Book","name":" The Long  Road","image":"https://www.royalroadcdn.com/public/covers-large/4242-the-long-road.jpg?time=1","author":{"@type":"Person","name":"A. Writer"},"url":"https://www.royalroad.com/fiction/4242/the-long-road","publisher":{"@type":"Organization","name":"Royal Road","url":"https://www.royalroad.com/"}}</script>
</head><body>
<div class="row fic-header">
  <div class="cover-art-container"><img class="thumbnail inline-block" data-type="cover" alt="The Long Road" src="https://www.royalroadcdn.com/public/covers-large/4242-the-long-road.jpg?time=1"></div>
  <div class="fic-title"><h1 class="font-white"> The Long  Road</h1>
    <h4 class="font-white"><span class="small font-white">by </span><span><a href="/profile/7" class="font-white">A. Writer</a></span></h4></div>
</div>
<table class="table no-border" id="chapters" data-chapters="3">
<thead><tr><th>Chapter Name</th><th>Release Date</th></tr></thead>
<tbody>
<tr style="cursor: pointer" data-url="/fiction/4242/the-long-road/chapter/9000/1-setting-out" data-volume-id="null" class="chapter-row">
  <td><a href="/fiction/4242/the-long-road/chapter/9000/1-setting-out">
       1. Setting   Out
  </a></td>
  <td class="text-right"><a href="/fiction/4242/the-long-road/chapter/9000/1-setting-out"><time>7 years </time> ago</a></td>
</tr>
<tr style="cursor: pointer" data-url="/fiction/4242/the-long-road/chapter/9001/2-lifes-little-problems" data-volume-id="null" class="chapter-row">
  <td><a href="/fiction/4242/the-long-road/chapter/9001/2-lifes-little-problems">2. Life&#x2019;s Little Problems</a></td>
  <td class="text-right"><a href="/fiction/4242/the-long-road/chapter/9001/2-lifes-little-problems"><time>7 years </time> ago</a></td>
</tr>
<tr style="cursor: pointer" data-url="/fiction/4242/the-long-road/chapter/9002/3-early" data-volume-id="null" class="chapter-row">
  <td><a href="/fiction/4242/the-long-road/chapter/9002/3-early">3. Early</a></td>
  <td class="text-right"><a href="/fiction/4242/the-long-road/chapter/9002/3-early"><time>1 day </time> ago</a></td>
</tr>
</tbody></table>
<script>
        window.fictionCover = "https://www.royalroadcdn.com/public/covers-large/4242-the-long-road.jpg?time=1";
        window.paidChapterDisplay = "obfuscate";
        window.chapters = [{"id":9000,"volumeId":null,"title":"1. Setting   Out","slug":"1-setting-out","date":"2018-10-28T21:34:43Z","order":0,"visible":1,"subscriptionTiers":null,"doesNotRollOver":false,"isUnlocked":true,"hiddenCount":null,"url":"/fiction/4242/the-long-road/chapter/9000/1-setting-out"},{"id":9001,"volumeId":null,"title":"2. Life’s Little Problems","slug":"2-lifes-little-problems","date":"2018-10-28T21:45:44Z","order":1,"visible":1,"subscriptionTiers":null,"doesNotRollOver":false,"isUnlocked":true,"hiddenCount":null,"url":"/fiction/4242/the-long-road/chapter/9001/2-lifes-little-problems"},{"id":9002,"volumeId":null,"title":"3. Early","slug":"3-early","date":"2026-10-05T00:00:00Z","order":2,"visible":1,"subscriptionTiers":[1],"doesNotRollOver":false,"isUnlocked":false,"hiddenCount":null,"url":"/fiction/4242/the-long-road/chapter/9002/3-early"}];

        window.volumes = [];
</script>
</body></html>`

func TestParseFiction(t *testing.T) {
	const pageURL = "https://www.royalroad.com/fiction/4242"
	const canonical = "https://www.royalroad.com/fiction/4242/the-long-road"
	const cover = "https://www.royalroadcdn.com/public/covers-large/4242-the-long-road.jpg?time=1"
	chapters := []ChapterRef{
		{ID: "9000", Title: "1. Setting Out", URL: canonical + "/chapter/9000/1-setting-out", Unlocked: true},
		{ID: "9001", Title: "2. Life’s Little Problems", URL: canonical + "/chapter/9001/2-lifes-little-problems", Unlocked: true},
		{ID: "9002", Title: "3. Early", URL: canonical + "/chapter/9002/3-early", Unlocked: false},
	}
	// The table doesn't say which chapters are locked.
	fromTable := append([]ChapterRef(nil), chapters...)
	fromTable[2].Unlocked = true

	cut := func(page, from, to string) string {
		i := strings.Index(page, from)
		j := strings.Index(page[i:], to)
		if i < 0 || j < 0 {
			t.Fatalf("fixture has no %q...%q", from, to)
		}
		return page[:i] + page[i+j+len(to):]
	}
	noLD := cut(fictionPageHTML, `<script type="application/ld+json">`, `</script>`)
	noMeta := strings.NewReplacer(`<meta `, `<x-meta `, `<link `, `<x-link `).Replace(noLD)
	noScript := cut(fictionPageHTML, `window.chapters = `, "];\n")
	noList := cut(noScript, `<table class="table no-border" id="chapters"`, `</table>`)
	noCover := strings.ReplaceAll(fictionPageHTML, cover, "/dist/img/nocover-new-min.png")
	noCover = strings.ReplaceAll(noCover, `"image":"/dist`, `"image":"https://www.royalroad.com/dist`)

	tests := []struct {
		name     string
		page     string
		want     Fiction
		chapters []ChapterRef
		wantErr  string
	}{
		{name: "full page", page: fictionPageHTML,
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: canonical, CoverURL: cover}, chapters: chapters},
		{name: "meta tags only", page: noLD,
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: canonical, CoverURL: cover}, chapters: chapters},
		{name: "header only", page: noMeta,
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: pageURL, CoverURL: cover}, chapters: chapters},
		{name: "chapter table only", page: noScript,
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: canonical, CoverURL: cover}, chapters: fromTable},
		{name: "no cover", page: noCover,
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: canonical}, chapters: chapters},
		{name: "no chapters published", page: strings.Replace(noList, "window.volumes", "window.chapters = [];\nwindow.volumes", 1),
			want: Fiction{Title: "The Long Road", Author: "A. Writer", URL: canonical, CoverURL: cover}, chapters: []ChapterRef{}},
		{name: "no chapter list", page: noList, wantErr: "no chapter list"},
		{name: "another page", page: `<html><head><title>Best Rated | Royal Road</title></head><body><h1>Best Rated</h1></body></html>`,
			wantErr: "not a Royal Road fiction page"},
		{name: "chapter without a url", page: strings.Replace(fictionPageHTML, `"url":"/fiction/4242/the-long-road/chapter/9001/2-lifes-little-problems"`, `"url":null`, 1),
			wantErr: "no id or URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFiction([]byte(tt.page), pageURL)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one saying %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != tt.want.Title || got.Author != tt.want.Author || got.URL != tt.want.URL || got.CoverURL != tt.want.CoverURL || got.ID != "" {
				t.Errorf("fiction = %+v\nwant %+v", *got, tt.want)
			}
			if len(got.Chapters) != len(tt.chapters) {
				t.Fatalf("chapters = %+v\nwant %+v", got.Chapters, tt.chapters)
			}
			for i, c := range got.Chapters {
				if !reflect.DeepEqual(c, tt.chapters[i]) {
					t.Errorf("chapter %d = %+v\nwant %+v", i, c, tt.chapters[i])
				}
			}
		})
	}
}

// chapterPage lays text out the way a chapter page does: a style block
// hiding one class, the title, a note before and after, ads, and the text
// in .chapter-content.
func chapterPage(text string) string {
	return `<!DOCTYPE html><html><head><title>1. Setting Out - The Long Road | Royal Road</title>
        <style>
            .cmHIDDENHIDDENHIDDEN{
                display: none;
                speak: never;
            }
        </style>
<style>.author-note-portlet hr, .chapter-content hr { display:block; border:0px; height:40px; }
@media print { .cnPRINT { display: none } }
/* .cnCOMMENT { display: none } */
.cnSHOWN { display: none; display: block }</style>
</head><body>
<div class="row fic-header"><h1 style="margin-top: 10px" class="font-white break-word">1. Setting Out</h1></div>
<div class="portlet light chapter"><div class="portlet-body">
<div class="portlet solid author-note-portlet"><div class="portlet-title"><div class="caption"><span class="caption-subject bold uppercase">A note from A. Writer</span></div></div>
  <div class="portlet-body author-note"><p>Note before: join my Patreon.</p></div></div>
<div class="margin-bottom-20 portlet light dKKumh"><div class="bold uppercase">Advertisement</div></div>
<div class="chapter-inner chapter-content">
` + text + `
</div>
<div class="portlet light dKKumh"><div class="bold uppercase">Advertisement</div></div>
<div class="portlet solid author-note-portlet"><div class="portlet-body author-note"><p>Note after: thanks for reading.</p></div></div>
</div></div>
</body></html>`
}

func renderChildren(t *testing.T, n *html.Node) string {
	t.Helper()
	var buf bytes.Buffer
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&buf, c); err != nil {
			t.Fatal(err)
		}
	}
	// Whitespace between blocks isn't of interest.
	return strings.ReplaceAll(strings.Join(strings.Fields(buf.String()), " "), "> <", "><")
}

func TestCleanChapter(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		text   string
		want   string
		images int
	}{
		{
			name:  "hidden notice between paragraphs",
			title: "Interlude",
			text: `<p class="cnAAAAAAAAAAAAAAAAAAAA">The road was long.</p><span class="cmHIDDENHIDDENHIDDEN"><br>Stolen content warning: this content belongs on Royal Road. Report any occurrences.<br></span>
<p class="cnBBBBBBBBBBBBBBBBBBBB">“Are we there yet?” Sam asked.</p>`,
			want: `<p>The road was long.</p><p>“Are we there yet?” Sam asked.</p>`,
		},
		{
			name:  "hidden notice of any element, anywhere",
			title: "Interlude",
			text: `<div><div style="padding-bottom: 1.5em"><p class="cnA">One.</p><p class="cnB cmHIDDENHIDDENHIDDEN">Taken without consent.</p>
<p class="cnC">Two <span class="cmHIDDENHIDDENHIDDEN">stolen</span>words.</p></div></div>`,
			want: `<div><div><p>One.</p><p>Two words.</p></div></div>`,
		},
		{
			name:  "only what the page's style hides",
			title: "Interlude",
			text: `<p class="cnPRINT">Print.</p><p class="cnCOMMENT">Comment.</p><p class="cnSHOWN">Shown.</p><p class="cmOTHEROTHEROTHER">Other.</p>
<p style="display:none">Inline.</p><p style="DISPLAY : NONE !important">Important.</p><p hidden>Attribute.</p><p style="display: block">Block.</p>`,
			want: `<p>Print.</p><p>Comment.</p><p>Shown.</p><p>Other.</p><p>Block.</p>`,
		},
		{
			name:  "presentational attributes, emphasis and breaks",
			title: "Interlude",
			text: `<p class="cnA" style="text-align: center" id="x" data-x="1" onclick="x()"><span style="font-weight: 400">Plain </span><em><span style="font-weight: 400">leaning</span></em> and <strong>bold</strong><br>next line</p>
<hr><blockquote class="q"><p>[Mission: Complete]</p></blockquote>`,
			want: `<p><span>Plain </span><em><span>leaning</span></em> and <strong>bold</strong><br/>next line</p><hr/><blockquote><p>[Mission: Complete]</p></blockquote>`,
		},
		{
			name:  "table",
			title: "Interlude",
			text: `<div style="max-width: 100%; overflow:auto"><table style="border-collapse: collapse; width: 100%" border="1"><tbody>
<tr><td style="width: 50%" colspan="2" class="c">Status</td></tr><tr><td rowspan="1">Name</td><td>Sam</td></tr></tbody></table></div>`,
			want: `<div><table><tbody><tr><td colspan="2">Status</td></tr><tr><td rowspan="1">Name</td><td>Sam</td></tr></tbody></table></div>`,
		},
		{
			name:  "table cells wrapped in a paragraph",
			title: "Interlude",
			text: `<div><div style="max-width: 100%; overflow:auto"><table style="width: 100%"><tbody>
<tr><td style="width: 25%">
<p class="cnA"><span style="font-weight: 400">Name</span></p>
</td><td><div><p>Type</p></div></td><td>&nbsp;</td></tr>
<tr><td><p>Debugger</p><p>(old)</p></td><td>Passive <p>Skill</p></td><th><p><em>Trait</em><br>Rare</p></th></tr></tbody></table></div></div>`,
			want: `<div><div><table><tbody><tr><td><span>Name</span></td><td>Type</td><td></td></tr><tr><td><p>Debugger</p><p>(old)</p></td><td>Passive <p>Skill</p></td><th><em>Trait</em><br/>Rare</th></tr></tbody></table></div></div>`,
		},
		{
			name:  "images",
			title: "Interlude",
			text: `<p class="cnA"><img style="display: block; margin-left: auto" src="https://i.example.com/map.png" alt="A map" width="640" height="450"></p>
<p><img src="/rel/b.png"><img src=""><img alt="none"><img src="data:image/png;base64,AAAA"><img src="  "></p><p><img src=""></p><p>After.</p>`,
			want:   `<p><img src="https://i.example.com/map.png" alt="A map"/></p><p><img src="/rel/b.png"/></p><p>After.</p>`,
			images: 2,
		},
		{
			name:  "scripts, styles, forms and embeds",
			title: "Interlude",
			text: `<p>Text.</p><script>alert(1)</script><style>p{color:red}</style><noscript>No script.</noscript><!-- comment -->
<form action="/x"><input name="a"><button>Vote</button></form><iframe src="https://example.com/"></iframe><video src="a.mp4">Video.</video>
<div class="portlet solid author-note-portlet"><div class="portlet-body author-note"><p>A note inside.</p></div></div><p>More.</p>`,
			want: `<p>Text.</p><p>More.</p>`,
		},
		{
			name:  "links",
			title: "Interlude",
			text: `<p>See <a href="https://example.com/map" rel="nofollow" target="_blank">the map</a>, <a href="/fiction/4242/the-long-road/chapter/9000/1-setting-out">chapter one</a>,
<a href="javascript:void(0)">this</a> and <a href="/cdn-cgi/l/email-protection" class="__cf_email__" data-cfemail="1b68727f5b7e637a766b777e35787476">[email&#160;protected]</a>.</p>`,
			want: `<p>See <a href="https://example.com/map">the map</a>, <a>chapter one</a>, <a>this</a> and sid@example.com.</p>`,
		},
		{
			name:  "spoiler keeps its text, not its frame",
			title: "Interlude",
			text: `<p class="cnA">The Players!</p>
<div class="spoiler">
<div class="smalltext" style="margin-bottom: 2px"><strong>Spoiler</strong>: Spoiler<input class="spoilerButton" type="button" value="Show"></div>
<div class="spoilerContent">
<div class="spoiler-inner" style="display: none">
<p class="cnB">Powered: the magic folks.</p>
<p class="cnC">Primos: the rest of us.</p><span class="cmHIDDENHIDDENHIDDEN"><br>Stolen content.<br></span>
</div>
</div>
</div>
<p class="cnD">After.</p>`,
			want: `<p>The Players!</p><div><div><div><p>Powered: the magic folks.</p><p>Primos: the rest of us.</p></div></div></div><p>After.</p>`,
		},
		{
			name:  "spoiler with the author's title",
			title: "Interlude",
			text: `<div class="spoiler">
<div class="smalltext" style="margin-bottom: 2px"><strong>Spoiler</strong>: V&amp;V stats <input class="spoilerButton" type="button" value="Show"></div>
<div class="spoilerContent">
<div class="spoiler-inner" style="display: none">
<div style="max-width: 100%; overflow:auto"><table style="width: 100%" border="1"><tbody><tr><td><p class="cnA"><span style="font-weight: 400">Health Points</span></p></td><td><p class="cnB">6,965</p></td></tr></tbody></table></div>
</div>
</div>
</div>
<div class="spoiler"><div class="smalltext" style="margin-bottom: 2px"><strong>Thanks to my patrons: </strong><input class="spoilerButton" type="button" value="Show"></div>
<div class="spoilerContent"><div class="spoiler-inner" style="display: none"><p>Ann, Bob.</p></div></div></div>`,
			want: `<div><div>V&amp;V stats </div><div><div><div><table><tbody><tr><td><span>Health Points</span></td><td>6,965</td></tr></tbody></table></div></div></div></div>` +
				`<div><div><strong>Thanks to my patrons: </strong></div><div><div><p>Ann, Bob.</p></div></div></div>`,
		},
		{
			name:  "spoiler from the forum days",
			title: "Interlude",
			text: `<span style="font-weight: bold;">Status Window Hall:</span>
<br /><div class='spoiler'><div class='smalltext' style='margin-bottom:2px'><b>Spoiler</b> : <input type='button' value='Show' class='spoilerButton'></div><div class='spoilerContent'><div style='display: none;'>
<br /><div style="text-align: center;"><table><tbody><tr><td width="25%"><span style="font-weight: bold;">Name:</span></td><td width="25%">Hall</td></tr></tbody></table></div></div></div></div>
<br />He closed the window.`,
			want: `<span>Status Window Hall:</span><br/><div><div><div><br/><div><table><tbody><tr><td><span>Name:</span></td><td>Hall</td></tr></tbody></table></div></div></div></div><br/>He closed the window.`,
		},
		{
			name:  "spoiler the browser folds",
			title: "Interlude",
			text: `<p class="cnA">Full Status:</p>
<div class="spoiler-new" data-class="spoiler" data-caption="Spoiler">
<p class="cnB"><strong>HP</strong>: 2/2</p>
</div>
<div class="spoiler-new" data-class="spoiler" data-caption="  Inventory ">
<p class="cnC">A rock.</p>
</div>
<p>He said <span class="inline-spoiler">the butler did it</span>.</p>`,
			want: `<p>Full Status:</p><div><p><strong>HP</strong>: 2/2</p></div><div><p>Inventory</p><p>A rock.</p></div><p>He said <span>the butler did it</span>.</p>`,
		},
		{
			name:  "hidden outside a spoiler",
			title: "Interlude",
			text: `<div class="spoilerContent"><div style="display: none"><p>Folded.</p></div></div>
<div class="spoiler"><div style="display: none"><p>Not a spoiler's body.</p></div></div>
<div class="spoiler-inner cmHIDDENHIDDENHIDDEN" style="display: none"><p>Hidden by the page.</p></div>`,
			want: `<div><div><p>Folded.</p></div></div>`,
		},
		{
			name:  "navigation lines between line breaks",
			title: "Interlude",
			text: `<a href="http://www.royalroadl.com/forum/viewtopic.php?f=119&t=1065&p=5737#p5737" target="_blank">Previous Chapter</a>
<br />
<br />Author's Note:
<br />So here it is.
<br />The food was definitely worth it though.
<br />
<br /><a href="http://www.royalroadl.com/forum/showthread.php?tid=6324" target="_blank">Next Chapter</a>
<br /><a href="http://www.royalroadl.com/fiction/494" target="_blank"><span style="font-weight: bold;">Review? :)</span></a>
<br /><a href="http://www.royalroadl.com/forum/viewtopic.php?f=64&t=97" target="_blank">&lt;&lt;Previous</a> <a href="http://www.royalroadl.com/forum/viewtopic.php?f=64&t=99" target="_blank">Next&gt;&gt;</a>
<br /><a href="http://www.royalroadl.com/forum/viewtopic.php?f=474&t=4268" target="_blank">[p=center]Next Chapter[/p]</a>
<br /><a href="/fiction/4242/the-long-road/chapter/8999/prologue">Prev</a> | <a href="https://www.royalroad.com/fiction/4242/the-long-road">Index</a> | <a href="https://royalroad.com/fiction/4242/the-long-road/chapter/9001/two">Next</a>`,
			want: `<br/><br/>Author&#39;s Note: <br/>So here it is. <br/>The food was definitely worth it though. <br/><br/><br/><br/><br/><br/>`,
		},
		{
			name:  "navigation lines in paragraphs",
			title: "Interlude",
			text: `<p class="cnA"><a href="http://www.royalroadl.com/forum/showthread.php?tid=10" target="_blank">Previous Chapter</a><br><br>Author's note:<br>Here we are.</p>
<p class="cnB">He closed the cabin's doors behind himself.<br><br><a href="http://www.royalroadl.com/forum/viewtopic.php?f=119&amp;t=875" target="_blank">Next Chapter &gt;&gt;</a>!</p>
<p class="cnC" style="text-align: center"><strong><a href="https://www.royalroad.com/fiction/4242/the-long-road/chapter/9001/two">Next Chapter</a></strong></p>`,
			want: `<p><br/><br/>Author&#39;s note:<br/>Here we are.</p><p>He closed the cabin&#39;s doors behind himself.<br/><br/></p>`,
		},
		{
			name:  "links that are not navigation",
			title: "Interlude",
			text: `<p>The road was long, as told in the <a href="https://www.royalroad.com/fiction/4242/the-long-road/chapter/8999/prologue">previous chapter</a>.</p>
<p><a href="https://example.com/next">Next Chapter</a></p>
<p><a href="https://www.patreon.com/someone">Next</a> | <a href="/fiction/4242/the-long-road/chapter/9001/two">Next</a></p>
<p><a href="http://www.royalroadl.com/forum/viewtopic.php?f=105&t=771">Chapter two The Hunt</a><br><a href="http://www.royalroadl.com/forum/showthread.php?tid=9">Map of Alfirin</a><br>
Chapter 5: 14,650 Words <a href="http://www.royalroadl.com/forum/showthread.php?tid=8">Released</a><br><a href="http://royalroadl.com/fiction/1679/">Prism</a><br>Next Chapter</p>
<p><a href="http://www.royalroadl.com/profile/7">Next</a> <a href="http://www.royalroadl.com/forum/showthread.php?tid=8"><img src="https://i.example.com/next.png" alt="Next"></a></p>`,
			want: `<p>The road was long, as told in the <a href="https://www.royalroad.com/fiction/4242/the-long-road/chapter/8999/prologue">previous chapter</a>.</p>` +
				`<p><a href="https://example.com/next">Next Chapter</a></p>` +
				`<p><a href="https://www.patreon.com/someone">Next</a> | <a>Next</a></p>` +
				`<p><a href="http://www.royalroadl.com/forum/viewtopic.php?f=105&amp;t=771">Chapter two The Hunt</a><br/><a href="http://www.royalroadl.com/forum/showthread.php?tid=9">Map of Alfirin</a><br/> Chapter 5: 14,650 Words <a href="http://www.royalroadl.com/forum/showthread.php?tid=8">Released</a><br/><a href="http://royalroadl.com/fiction/1679/">Prism</a><br/>Next Chapter</p>` +
				`<p><a href="http://www.royalroadl.com/profile/7">Next</a><a href="http://www.royalroadl.com/forum/showthread.php?tid=8"><img src="https://i.example.com/next.png" alt="Next"/></a></p>`,
			images: 1,
		},
		{
			name:  "navigation boxes",
			title: "Interlude",
			text: `Shit. I can't escape.
<br /><div style="text-align: center;"><div style="max-width: 100%; overflow:auto"><table><tbody><tr><td colspan='4'>Continue Misfortune?</td></tr><td width="25%"><a href="http://www.royalroadl.com/forum/viewtopic.php?f=161&t=1281" target="_blank">Previous</a></td><td colspan='2'><a href="http://www.royalroadl.com/forum/viewforum.php?f=161" target="_blank">Exit</a></td><td width="25%"><a href="http://www.royalroadl.com/forum/viewtopic.php?f=161&t=1293" target="_blank">Next</a></td></tbody></table></div></div>
<br />One.
<div style="max-width: 100%; overflow:auto"><table><tbody><tr><td colspan='4'>Onwards?</td></tr><td width="25%">Previous</td><td colspan='2'><a href="http://www.royalroadl.com/forum/viewforum.php?f=401" target="_blank">Exit</a></td><td width="25%"><a href="http://www.royalroadl.com/forum/showthread.php?tid=4070" target="_blank">Next</a></td></tbody></table></div>
<br />Two.
<div style="text-align: center;"><table><tbody><tr><td colspan='4'>Connect to Royal Road – Elementarem Vim next Chapter?</td></tr><tr><td colspan='2'><a href="http://www.royalroadl.com/forum/viewtopic.php?f=64&t=561" target="_blank">Yes</a></td><td colspan='2'><a href="http://www.royalroadl.com/forum/viewtopic.php?f=64&t=348" target="_blank">No</a></td></tr></tbody></table></div>
<br />Three.
<div style="text-align: center;"><table><tbody><tr><td colspan='4'>Do you want to connect to Novus Vita?</td></tr><tr><td colspan='2'><a href="http://www.royalroadl.com/forum/showthread.php?tid=3132" target="_blank">Yes</a></td><td colspan='2'>No</td></tr></tbody></table></div>
<br />Four.
<table><tbody><tr><td width="25%"><span style="font-weight: bold;"><span style="text-decoration: underline;"><a href="http://www.royalroadl.com/forum/viewtopic.php?f=501&t=5358" target="_blank">Previous</a></span></span></td><td width="25%"><a href="http://www.royalroadl.com/forum/viewforum.php?f=501" target="_blank">Main</a></td><td><a href="http://www.royalroadl.com/fiction/403" target="_blank">Back to Main Page</a></td><td><a href="http://www.royalroadl.com/forum/showthread.php?tid=3673" target="_blank">NEXT CHAPTER</a></td></tr></tbody></table>`,
			want: `Shit. I can&#39;t escape. <br/><br/>One. <br/>Two. <br/>Three. <br/>Four.`,
		},
		{
			name:  "tables that are not navigation boxes",
			title: "Interlude",
			text: `<table><tbody><tr><td colspan="2">Accept the quest?</td></tr><tr><td>Yes</td><td>No</td></tr></tbody></table>
<table><tbody><tr><td>Quest</td><td>Reward</td></tr><tr><td><a href="http://www.royalroadl.com/forum/showthread.php?tid=8">Next</a></td><td>10 gold</td></tr></tbody></table>
<table><tbody><tr><td><a href="http://www.royalroadl.com/forum/viewtopic.php?f=1&t=2">Chapter 1 - A New Beginning</a></td><td><a href="http://www.royalroadl.com/forum/viewtopic.php?f=1&t=3">Chapter 2 - The Return</a></td></tr></tbody></table>
<table><tbody><tr><td><a href="https://example.com/a">Yes</a></td><td><a href="https://example.com/b">No</a></td></tr></tbody></table>`,
			want: `<table><tbody><tr><td colspan="2">Accept the quest?</td></tr><tr><td>Yes</td><td>No</td></tr></tbody></table>` +
				`<table><tbody><tr><td>Quest</td><td>Reward</td></tr><tr><td></td><td>10 gold</td></tr></tbody></table>` +
				`<table><tbody><tr><td><a href="http://www.royalroadl.com/forum/viewtopic.php?f=1&amp;t=2">Chapter 1 - A New Beginning</a></td><td><a href="http://www.royalroadl.com/forum/viewtopic.php?f=1&amp;t=3">Chapter 2 - The Return</a></td></tr></tbody></table>` +
				`<table><tbody><tr><td><a href="https://example.com/a">Yes</a></td><td><a href="https://example.com/b">No</a></td></tr></tbody></table>`,
		},
		{
			name:  "space set apart in an element of its own",
			title: "Interlude",
			text:  `<p><em>I</em><span style="font-weight: 400"> </span>assure<span><br></span>you<strong></strong>.</p>`,
			want:  `<p><em>I</em> assure<br/>you.</p>`,
		},
		{
			name:  "title line",
			title: "1. Good Morning Brother",
			text: `
<p class="cnA" style="text-align: center"><strong>Chapter 001<br></strong><strong>Good Morning Brother</strong></p>
<p class="cnB">Zorian’s eyes abruptly shot open.</p>`,
			want: `<p>Zorian’s eyes abruptly shot open.</p>`,
		},
		{
			name:  "title over two lines",
			title: "Chapter 1: Warm Spots and Foolish Things",
			text:  `<div><h2>Chapter 1</h2><p><em>Warm Spots and Foolish Things</em></p><p>She was not the guild's cat.</p></div>`,
			want:  `<div><p>She was not the guild&#39;s cat.</p></div>`,
		},
		{
			name:  "first sentence that isn't the title",
			title: "1. Good Morning Brother",
			text:  `<p>“Good morning, brother!”</p><p>Zorian glared.</p>`,
			want:  `<p>“Good morning, brother!”</p><p>Zorian glared.</p>`,
		},
		{
			name:  "title only after other text",
			title: "Chapter 2",
			text:  `<p>Previously: nothing.</p><p>Chapter 2</p>`,
			want:  `<p>Previously: nothing.</p><p>Chapter 2</p>`,
		},
		{
			name:  "nothing but a hidden notice",
			title: "Interlude",
			text:  `<span class="cmHIDDENHIDDENHIDDEN"><br>Stolen.<br></span>`,
			want:  ``,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, images, err := cleanChapter([]byte(chapterPage(tt.text)), tt.title)
			if err != nil {
				t.Fatal(err)
			}
			if got := renderChildren(t, content); got != tt.want {
				t.Errorf("content:\n got %s\nwant %s", got, tt.want)
			}
			if len(images) != tt.images {
				t.Errorf("%d images, want %d", len(images), tt.images)
			}
			for _, img := range images {
				if img.Data != "img" || img.Parent == nil || attr(img, "src") == "" {
					t.Errorf("image %+v isn't an <img> left in the content", img)
				}
			}
		})
	}
}

func TestCleanChapterWithoutText(t *testing.T) {
	for name, page := range map[string]string{
		"locked":    `<!DOCTYPE html><html><body><div class="row fic-header"><h1>3. Early</h1></div><div class="portlet light"><p>This chapter is not available yet.</p></div></body></html>`,
		"not found": `<html><body><h1>404</h1></body></html>`,
		"empty":     ``,
	} {
		if content, _, err := cleanChapter([]byte(page), "3. Early"); err == nil {
			t.Errorf("%s: read as a chapter: %q", name, renderChildren(t, content))
		}
	}
}

func TestTitleRepeat(t *testing.T) {
	tests := []struct {
		line, title string
		want        repeat
	}{
		{"Chapter 001 Good Morning Brother", "1. Good Morning Brother", repeatsTitle},
		{"1. Good Morning Brother", "1. Good Morning Brother", repeatsTitle},
		{"GOOD MORNING BROTHER", "1. Good Morning Brother", repeatsName},
		{"Chapter 1", "Chapter 1: Warm Spots and Foolish Things", repeatsNumber},
		{"Warm Spots and Foolish Things", "Chapter 1: Warm Spots and Foolish Things", repeatsName},
		{"Chapter One: Warm Spots", "Chapter 1: Warm Spots", noRepeat},
		{"Chapter 12", "Chapter 12", repeatsTitle},
		{"12", "Chapter 12", repeatsTitle},
		{"Chapter 13", "Chapter 12", noRepeat},
		{"Chapter 2: Good Morning Brother", "1. Good Morning Brother", noRepeat},
		{"Life's Little Problems", "2. Life’s Little Problems", repeatsName},
		{"Chapter 1: Refuse the Chain", "GB - Chapter 1: Refuse The Chain", repeatsTitle},
		{"Chapter 5: The End", "Book 2, Chapter 5: The End", repeatsTitle},
		{"Chain", "GB - Chapter 1: Refuse The Chain", noRepeat},
		{"What's Left After?", "Chapter 4: What's Left After?", repeatsName},
		{"❈—04:: Case Zero", "❈—04:: Case Zero", repeatsTitle},
		{"Prologue", "Prologue", repeatsTitle},
		{"Run.", "Run", noRepeat},
		{"Run!", "3. Run", noRepeat},
		{"“Good Morning, Brother”", "1. Good Morning Brother", noRepeat},
		{"She said good morning brother", "1. Good Morning Brother", noRepeat},
		{"Good morning", "1. Good Morning Brother", noRepeat},
		{"The", "Chapter 3: The", repeatsName},
		{"Zorian’s eyes abruptly shot open.", "1. Good Morning Brother", noRepeat},
		{"1", "Good Morning Brother", noRepeat},
		{"Chapter", "Chapter 1", noRepeat},
		{"", "Chapter 1", noRepeat},
		{"Chapter 1", "", noRepeat},
	}
	for _, tt := range tests {
		if got := titleRepeat(tt.line, tt.title); got != tt.want {
			t.Errorf("titleRepeat(%q, %q) = %d, want %d", tt.line, tt.title, got, tt.want)
		}
	}
}

func TestHiddenClasses(t *testing.T) {
	tests := []struct {
		css  string
		want []string
	}{
		{".cmA{\n display: none;\n speak: never;\n }", []string{"cmA"}},
		{".a{display:none}.b { DISPLAY : None !important }", []string{"a", "b"}},
		{".a, .b , p.c, .d .e, #f { display: none }", []string{"a", "b"}},
		{".a { display: block } .b { visibility: hidden } .c { display: none; display: inline }", nil},
		{"@media (max-width: 10px) { .a { display: none } } .b { display: none }", []string{"b"}},
		{"/* .a { display: none } */ .b { color: red }", nil},
		{"@import url(x.css); .a { display: none }", []string{"a"}},
		{".a { display: none", []string{"a"}},
		{"", nil},
	}
	for _, tt := range tests {
		doc, err := html.Parse(strings.NewReader("<html><head><style>" + tt.css + "</style></head><body></body></html>"))
		if err != nil {
			t.Fatal(err)
		}
		got := hiddenClasses(doc)
		if len(got) != len(tt.want) {
			t.Errorf("hiddenClasses(%q) = %v, want %v", tt.css, got, tt.want)
			continue
		}
		for _, class := range tt.want {
			if !got[class] {
				t.Errorf("hiddenClasses(%q) = %v, want %v", tt.css, got, tt.want)
			}
		}
	}
}
