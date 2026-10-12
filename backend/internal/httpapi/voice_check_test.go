package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/voicecheck"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// The voice check hears each voiced character's reference clip, lists the
// ones that don't sound like their casting, hears a clip again only once
// it has been rendered again, and drops a finding the reader dismisses.
func TestVoiceCheck(t *testing.T) {
	srv, s, fake, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{
		{Title: "Ch", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatal(err)
	}
	book, _ = s.GetBook(bookID)
	// Oren's clip will be heard as his casting says, Tallie's as a man's;
	// Sid has a voice but no clip yet, and Bert no voice at all.
	castings := map[string]string{
		"Oren":   "Speak as a middle-aged dwarf man. Use a low, deep pitch.",
		"Tallie": "Speak as a mature woman. Her pitch is low and grounded.",
		"Sid":    "Speak as a young woman.",
	}
	presets := map[string]string{}
	for _, name := range []string{"Oren", "Tallie", "Sid", "Bert"} {
		c, _, err := s.UpsertCharacter(store.SeriesScope(book), name, false)
		if err != nil {
			t.Fatal(err)
		}
		if castings[name] == "" {
			continue
		}
		preset, err := s.CreateVoicePreset(name, castings[name], "ref", 7, 1.0, voices.DefaultDesignModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetCharacterVoice(c.ID, narration.BookCloneModel(book), preset.ID); err != nil {
			t.Fatal(err)
		}
		presets[name] = preset.ID
		if name == "Sid" {
			continue
		}
		path := audiopath.VoicePresetRefFile(srv.DataDir, preset.ID)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// The clip's bytes say who it is, for the fake listener below.
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake.OnListen = func(req ttsproto.ListenRequest) (string, error) {
		tallie := req.AudioBase64 == "VGFsbGll" // base64("Tallie")
		switch {
		case strings.Contains(req.Question, "man or a woman"):
			return "man", nil
		case strings.Contains(req.Question, "child, a teenager"):
			return "middle-aged", nil
		case strings.Contains(req.Question, "high-pitched"):
			if tallie {
				return "high-pitched", nil
			}
			return "The speaker's voice is low and deep.", nil
		}
		return "English.", nil
	}

	url := ts.URL + "/api/books/" + bookID + "/voice-check"
	get := func(query string) voiceCheckResponse {
		t.Helper()
		var got voiceCheckResponse
		if resp := doJSON(t, http.MethodGet, url+query, nil, &got); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET voice-check: %d", resp.StatusCode)
		}
		return got
	}
	run := func(wantChecked int) voiceCheckResponse {
		t.Helper()
		if resp := doJSON(t, http.MethodPost, url+"/run", nil, nil); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST run: %d", resp.StatusCode)
		}
		var got voiceCheckResponse
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if got = get(""); got.Checked == wantChecked {
				return got
			}
		}
		t.Fatalf("checked %d voices, want %d", got.Checked, wantChecked)
		return got
	}

	if got := get(""); got.Voices != 2 || got.Checked != 0 || len(got.Findings) != 0 {
		t.Fatalf("before a run: %+v", got)
	}
	got := run(2)
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want Tallie alone", got.Findings)
	}
	f := got.Findings[0]
	if f.Speaker != "Tallie" || f.PresetID != presets["Tallie"] || f.Heard.Gender != voicecheck.GenderMan || f.Cast.Gender != voicecheck.GenderWoman {
		t.Errorf("finding = %+v", f)
	}
	if want := []voiceTrait{voicecheck.TraitGender, voicecheck.TraitPitch}; len(f.Mismatches) != 2 || f.Mismatches[0] != want[0] || f.Mismatches[1] != want[1] {
		t.Errorf("mismatches = %v, want %v", f.Mismatches, want)
	}
	if f.Heard.Accent != "English" {
		t.Errorf("heard accent = %q", f.Heard.Accent)
	}
	if all := get("?all=1"); len(all.Findings) != 2 {
		t.Errorf("?all=1 lists %d voices, want 2", len(all.Findings))
	}
	asked := len(fake.ListenCalls())
	if asked != 2*len(voicecheck.Questions) {
		t.Errorf("asked %d questions, want %d", asked, 2*len(voicecheck.Questions))
	}

	// Nothing has been rendered since: a second run asks nothing.
	run(2)
	if n := len(fake.ListenCalls()); n != asked {
		t.Errorf("a second run asked %d more questions", n-asked)
	}

	// Tallie's clip rendered again is a voice not yet heard.
	if err := os.WriteFile(audiopath.VoicePresetRefVersionFile(srv.DataDir, presets["Tallie"]), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := get(""); got.Checked != 1 || len(got.Findings) != 0 {
		t.Errorf("after a re-render: %+v", got)
	}
	got = run(2)
	if len(got.Findings) != 1 {
		t.Fatalf("after hearing the new clip: %+v", got.Findings)
	}

	resp := doJSON(t, http.MethodPost, url+"/dismiss", dismissVoiceCheckRequest{PresetID: presets["Tallie"]}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss: %d", resp.StatusCode)
	}
	if got := get(""); len(got.Findings) != 0 || got.Checked != 2 {
		t.Errorf("after dismissing: %+v", got)
	}
	if resp := doJSON(t, http.MethodPost, url+"/dismiss", dismissVoiceCheckRequest{PresetID: "nobody"}, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("dismissing an unchecked voice: %d", resp.StatusCode)
	}
}

// POST /api/voices/listen asks one question of one voice's clip, through
// the job queue.
func TestListen(t *testing.T) {
	srv, s, fake, ts := newTestServer(t)
	preset, err := s.CreateVoicePreset("Oren", "Speak as a dwarf.", "ref", 7, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatal(err)
	}
	path := audiopath.VoicePresetRefFile(srv.DataDir, preset.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.OnListen = func(req ttsproto.ListenRequest) (string, error) {
		return "heard: " + req.Question, nil
	}
	var got llmTestResponse
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/voices/listen", listenRequest{PresetID: preset.ID, Question: "Who speaks?"}, &got)
	if resp.StatusCode != http.StatusOK || got.Text != "heard: Who speaks?" {
		t.Errorf("listen: %d %q", resp.StatusCode, got.Text)
	}
	for name, tc := range map[string]struct {
		body listenRequest
		want int
	}{
		"no question":  {listenRequest{PresetID: preset.ID}, http.StatusBadRequest},
		"no clip":      {listenRequest{PresetID: "missing", Question: "Who?"}, http.StatusNotFound},
		"a path":       {listenRequest{PresetID: "../library", Question: "Who?"}, http.StatusBadRequest},
		"no preset id": {listenRequest{Question: "Who?"}, http.StatusBadRequest},
	} {
		if resp := doJSON(t, http.MethodPost, ts.URL+"/api/voices/listen", tc.body, nil); resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
}
