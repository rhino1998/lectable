package epub

import (
	"fmt"
	"strings"
	"testing"
)

// playBlocks parses body as one chapter and applies the play pass.
func playBlocks(t *testing.T, body string, mode PlayMode) (blocks []Block, play bool) {
	t.Helper()
	_, _, blocks, _ = extractChapter([]byte("<html><body>"+body+"</body></html>"), nil, "", nil)
	chapters := []Chapter{{Blocks: blocks}}
	play = applyPlay(chapters, mode)
	return chapters[0].Blocks, play
}

// render is one line per block: speaker, unspoken label and text.
func render(blocks []Block) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Kind != BlockText {
			fmt.Fprintf(&b, "<%s>\n", bl.Kind)
			continue
		}
		inline := ""
		if bl.Inline {
			inline = "+ "
		}
		who := "-"
		if bl.IsQuote {
			who = bl.Speaker
		}
		fmt.Fprintf(&b, "%s%s {%s} %s\n", inline, who, bl.Label, bl.Text)
	}
	return b.String()
}

func TestPlayLabelForms(t *testing.T) {
	cases := []struct{ name, html, want string }{
		{
			"label on its own line, a direction between two paragraphs of one speech",
			`<h3>SCENE I.</h3>
			<p class="scenedesc">Enter <span class="charname">Theseus</span> and Hippolyta.</p>
			<p class="drama">THESEUS.<br/>Now, fair Hippolyta,<br/>our nuptial hour</p>
			<p class="drama">HIPPOLYTA.<br/>Four days will quickly steep.</p>
			<p class="drama">THESEUS.<br/>Go, Philostrate.</p>
			<p class="right">[<i>Exit Philostrate.</i>]</p>
			<p class="drama">Hippolyta, I woo’d thee (messengers of youth) with my sword.</p>
			<p class="drama">THESEUS.<br/>Thanks.</p>`,
			`- {} SCENE I.
- {} Enter Theseus and Hippolyta.
Theseus {THESEUS.} Now, fair Hippolyta, our nuptial hour
Hippolyta {HIPPOLYTA.} Four days will quickly steep.
Theseus {THESEUS.} Go, Philostrate.
- {} [Exit Philostrate.]
Theseus {} Hippolyta, I woo’d thee (messengers of youth) with my sword.
Theseus {THESEUS.} Thanks.
`,
		},
		{
			"inline capitals, honorifics, a delivery direction and an action",
			`<p>MRS. PEARCE [hesitating] A young woman wants to see you, sir.</p>
			<p>HIGGINS. Show her up. [He rushes across to his table] What is it?</p>
			<p class="stage">Freddy rushes in out of the rain.</p>
			<p>MRS. PEARCE. Very well, sir.</p>
			<p>HIGGINS. Good.</p><p>HIGGINS. Yes.</p><p>MRS. PEARCE. No.</p>`,
			`Mrs. Pearce {MRS. PEARCE [hesitating]} A young woman wants to see you, sir.
Higgins {HIGGINS.} Show her up.
+ - {} [He rushes across to his table]
+ Higgins {} What is it?
- {} Freddy rushes in out of the rain.
Mrs. Pearce {MRS. PEARCE.} Very well, sir.
Higgins {HIGGINS.} Good.
Higgins {HIGGINS.} Yes.
Mrs. Pearce {MRS. PEARCE.} No.
`,
		},
		{
			"label in a small-caps span, italic parenthesized directions",
			`<p><span class="smcap">Helena.</span> (<i>Watching his gestures</i>) Mayn’t we talk?</p>
			<p><span class="smcap">Domin.</span> Come in. (<i>He crosses the room</i>)</p>
			<p><span class="smcap">Helena.</span> Yes.</p><p><span class="smcap">Domin.</span> No.</p>
			<p><span class="smcap">Helena.</span> Well.</p><p><span class="smcap">Domin.</span> So.</p>`,
			`Helena {Helena. (Watching his gestures)} Mayn’t we talk?
Domin {Domin.} Come in.
+ - {} (He crosses the room)
Helena {Helena.} Yes.
Domin {Domin.} No.
Helena {Helena.} Well.
Domin {Domin.} So.
`,
		},
		{
			"a scene in one paragraph, labels alone between double line breaks",
			`<p>FAUST<br/><br/>(<i>He turns.</i>)<br/><br/>I’ve studied now Philosophy<br/>And Medicine.<br/><br/>
			A STUDENT<br/><br/>A short time, only, am I here.<br/><br/>FAUST<br/><br/>Your courtesy.<br/><br/>
			FAUST<br/><br/>Again.</p>`,
			`- {} (He turns.)
Faust {FAUST} I’ve studied now Philosophy And Medicine.
A Student {A STUDENT} A short time, only, am I here.
Faust {FAUST} Your courtesy.
Faust {FAUST} Again.
`,
		},
		{
			"a direction on the label, an italic bracketed one opening the speech",
			`<p class="drama">CYRANO (delighted):<br/>Come back!</p>
			<p class="drama">NORA.<br/><i>[drops her cloak]</i>. Someone is coming now! <i>[Goes to the door.]</i> No.</p>
			<p class="drama">CYRANO:<br/>Two!</p><p class="drama">CYRANO:<br/>Three!</p>
			<p class="drama">NORA.<br/>Yes.</p><p class="drama">NORA.<br/>No.</p>`,
			`Cyrano {CYRANO (delighted):} Come back!
Nora {NORA. [drops her cloak]} Someone is coming now!
+ - {} [Goes to the door.]
+ Nora {} No.
Cyrano {CYRANO:} Two!
Cyrano {CYRANO:} Three!
Nora {NORA.} Yes.
Nora {NORA.} No.
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks, play := playBlocks(t, tc.html, PlayOn)
			if !play {
				t.Fatal("not read as a play")
			}
			if got := render(blocks); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			for _, b := range blocks {
				if b.src != nil {
					t.Errorf("block %q still holds its source", b.Text)
				}
			}
		})
	}
}

// Parentheses are directions only in a book that sets its directions in
// them; elsewhere they are spoken.
func TestPlayParenDirections(t *testing.T) {
	speech := `<p>CYRANO: Two! (He draws.) Three!</p><p>CYRANO: One.</p><p>CYRANO: Go.</p>`
	blocks, _ := playBlocks(t, speech, PlayOn)
	if got := render(blocks[:1]); got != "Cyrano {CYRANO:} Two! (He draws.) Three!\n" {
		t.Errorf("spoken parentheses: got %s", got)
	}
	blocks, _ = playBlocks(t, speech+strings.Repeat(`<p>(Bellerose comes forward and bows.)</p>`, minParenDirections), PlayOn)
	want := "Cyrano {CYRANO:} Two!\n+ - {} (He draws.)\n+ Cyrano {} Three!\n"
	if got := render(blocks[:3]); got != want {
		t.Errorf("parenthesized directions: got %s", got)
	}
	if last := blocks[len(blocks)-1]; last.IsQuote {
		t.Errorf("a parenthesized paragraph was read as speech: %+v", last)
	}
}

// A character with a single line is still a speaker when their label looks
// like everyone else's and sits among recurring speakers'.
func TestPlayRareSpeaker(t *testing.T) {
	blocks, _ := playBlocks(t, `<p>TRANSCRIBER’S NOTE: apostrophes were omitted.</p><p>A drawing room.</p>
		<p>JACK. One.</p><p>PORTER. Sixpence.</p><p>JACK. Two.</p><p>JACK. Three.</p>`, PlayOn)
	want := `- {} TRANSCRIBER’S NOTE: apostrophes were omitted.
- {} A drawing room.
Jack {JACK.} One.
Porter {PORTER.} Sixpence.
Jack {JACK.} Two.
Jack {JACK.} Three.
`
	if got := render(blocks); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlayDetection(t *testing.T) {
	var play, novel strings.Builder
	for i := 0; i < 30; i++ {
		play.WriteString(`<p>JACK. I have lost both my parents.</p><p>LADY BRACKNELL. That looks like carelessness.</p>`)
		novel.WriteString(`<p>“I have lost both my parents,” Jack said.</p><p>Lady Bracknell frowned.</p>`)
	}
	// What a LitRPG scatters through its prose: recurring, but nowhere
	// near most of the book.
	novel.WriteString(`<p>WARNING: low health.</p><p>WARNING: low mana.</p><p>WARNING: level up.</p>`)

	if blocks, ok := playBlocks(t, play.String(), PlayAuto); !ok || blocks[0].Speaker != "Jack" {
		t.Errorf("a play wasn't read as one (play=%v, first block %+v)", ok, blocks[0])
	}
	blocks, ok := playBlocks(t, novel.String(), PlayAuto)
	if ok {
		t.Error("a novel was read as a play")
	}
	if !blocks[0].IsQuote || blocks[0].Speaker != "" || blocks[0].Text != "“I have lost both my parents,”" {
		t.Errorf("a novel's blocks changed: %+v", blocks[0])
	}
	if _, ok := playBlocks(t, play.String(), PlayOff); ok {
		t.Error("PlayOff still read a play")
	}
}
