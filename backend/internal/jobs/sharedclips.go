package jobs

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/clipkey"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/voicefx"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// Shared clips: a narration clip is stored once, library-wide, under a
// content key (internal/clipkey) - what is spoken, by which reference
// clip, through which clone model - at audiopath.SharedClipFile, and the
// paragraph_audio row of every paragraph it plays for names that key
// (clip_key). Another paragraph reading the same line the same way -
// "Fritz said." later in the book, the same narrator line in another
// book - is put on the existing clip instead of being rendered again.
// That covers single paragraphs, scare-quote merge groups (keyed by the
// whole group's spoken text, members pointing into the clip as ever) and
// unvoiced lines (silence, shared by spelling).
//
// A clip is found by its key alone. While a paragraph is ready on it, that
// row supplies its duration and word timings; once the last row is gone (a
// voice change, a reimport, a re-attribution) the clip is kept for
// clipkey.OrphanRetention with what that row knew (store.ClipOrphan), so
// the same reading wanted again - the voice switched back, the chapter
// reimported unchanged - is still not rendered twice. Explicitly clearing
// a chapter's or a book's audio is the exception: that asks for new
// renders, so the clips it leaves unused are deleted on the spot
// (DiscardOrphanedClips). "Regenerate this paragraph" renders the clip
// anew for every paragraph using it: whatever was wrong with it is wrong
// wherever it plays.
//
// Every narration clip is stored this way; nothing is written to a
// paragraph's own path (audiopath.ParagraphFile) any more except as a
// fallback when the store can't be written.

// sharedClips is a Manager's hold on the shared store.
type sharedClips struct {
	// mu serializes changes to the store, so a clip is never replaced
	// between being found and being linked to.
	mu sync.Mutex

	renderMu sync.Mutex
	// rendering holds, per content key, a channel closed once the task
	// rendering that clip has settled (claim/release).
	rendering map[string]chan struct{}
}

// claim makes the caller the one task rendering key's clip, or returns
// the channel to wait on while another does - two tasks for the same line
// would otherwise both render it and one result be thrown away.
func (c *sharedClips) claim(key string) (wait <-chan struct{}) {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()
	if ch, ok := c.rendering[key]; ok {
		return ch
	}
	if c.rendering == nil {
		c.rendering = map[string]chan struct{}{}
	}
	c.rendering[key] = make(chan struct{})
	return nil
}

func (c *sharedClips) release(key string) {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()
	if ch, ok := c.rendering[key]; ok {
		close(ch)
		delete(c.rendering, key)
	}
}

// claimSharedClip decides whether t renders its clip: not if t.clipKey's
// shared clip already exists (t.clipHit), which it waits for while another
// task is rendering it; otherwise t becomes the task rendering it
// (t.claimed, released by handleResult once t's own result is settled). A
// fresh task always renders, once no other render of the clip is running.
func (m *Manager) claimSharedClip(ctx context.Context, t *task) {
	members := taskMembers(t)
	for {
		if !t.fresh {
			m.clips.mu.Lock()
			t.clipHit = m.alreadyOn(t.clipKey, members[0], t.voiceID) || m.sharedClip(t.clipKey, members, t.voiceID) != nil
			m.clips.mu.Unlock()
			if t.clipHit {
				return
			}
		}
		wait := m.clips.claim(t.clipKey)
		if wait == nil {
			t.claimed = t.clipKey
			return
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return
		}
	}
}

// taskClipKey is the content key of the clip t renders, given the lexicon
// its text is resolved with and the emotion variant it clones from ("" for
// the base clip - see emotionVariant).
func (m *Manager) taskClipKey(t *task, lexicon []pronounce.LexiconEntry, emotion string) string {
	if unvoicedTask(t) {
		return silenceRecipe(t.paragraph.Text).Key(0)
	}
	members := taskMembers(t)
	alignText := t.paragraph.Text
	if len(members) > 1 {
		alignText = mergedAlignText(members)
	}
	if t.kind == KindVoiceDesign {
		text := mergedGenerationText(members, "", lexicon)
		return withEffect(designRecipe(t.designModel, t.instruct, t.seed, t.language, text, alignText), t.effect).Key(t.take)
	}
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	text := mergedGenerationText(members, cloneModel, lexicon)
	return withEffect(m.clipRecipe(cloneModel, t.presetID, emotion, t.language, t.cloneInstruct, text, alignText), t.effect).Key(t.take)
}

// resolveAtEnqueue puts t's paragraphs on their clip without queueing t at
// all, when that clip already exists and t waits on nothing: a repeated
// line then goes ready the moment it is asked for, instead of taking its
// turn in the generation queue only to find its clip there. A task with
// something still to wait for (a pronunciation or emotion pass, a
// reference clip or emotion variant not rendered yet) is queued as usual -
// what it would read may still change - and looks again when it runs.
func (m *Manager) resolveAtEnqueue(t *task) bool {
	if !t.shared || t.fresh || !m.waitsOnNothing(t) {
		return false
	}
	emotion, _, _, _ := m.emotionVariant(t)
	t.clipKey = m.taskClipKey(t, m.lexiconFor(t.bookID), emotion)
	if !m.useSharedClip(t) {
		t.clipKey = ""
		return false
	}
	return true
}

// waitsOnNothing reports whether t, queued now, would have no dependency
// to wait for: none that has to be created, and none already in the queue.
func (m *Manager) waitsOnNothing(t *task) bool {
	for _, need := range m.needs(t) {
		if need.New != nil {
			return false
		}
		if _, queued := m.sched.Priority(need.Key); queued {
			return false
		}
	}
	return true
}

// alreadyOn reports whether anchor is ready on the shared clip key under
// voiceID as it is - which a task finds when its paragraph was put on the
// clip after it was asked for (another member of its merge group resolved
// the whole group, say). There is nothing left for it to do.
func (m *Manager) alreadyOn(key string, anchor store.Paragraph, voiceID string) bool {
	states, err := m.store.ParagraphAudioStatuses([]string{anchor.ID}, voiceID)
	if err != nil {
		return false
	}
	st := states[anchor.ID]
	return st.Status == store.AudioReady && st.ClipKey == key
}

// clipPart is one paragraph's share of a clip: where its words start in
// it, how long they run, and their timings (raw JSON). A clip of one
// paragraph has one part, the whole of it; a scare-quote merge group's has
// one per member, the first being the anchor's.
type clipPart struct {
	pointerSeconds float64
	duration       float64
	wordTimings    string
}

// taskMembers is the paragraphs t's one clip is for, anchor first.
func taskMembers(t *task) []store.Paragraph {
	if len(t.mergeParagraphs) > 1 {
		return t.mergeParagraphs
	}
	return []store.Paragraph{t.paragraph}
}

// mergedAlignText is a merge group's recipe AlignText: its members' plain
// texts, kept apart, so two groups share a clip only when they split the
// same words the same way.
func mergedAlignText(members []store.Paragraph) string {
	texts := make([]string, len(members))
	for i, p := range members {
		texts[i] = p.Text
	}
	return strings.Join(texts, "\n")
}

// designRecipe is the recipe of a paragraph rendered straight from a voice
// instruction, with no preset (KindVoiceDesign).
func designRecipe(designModel, instruct string, seed int, language, text, alignText string) clipkey.Recipe {
	return clipkey.Recipe{
		CloneModel: "design:" + designModel,
		Reference:  instruct + "\x00" + strconv.Itoa(seed),
		Language:   language,
		Text:       text,
		AlignText:  alignText,
	}
}

// voiceRecipe is the recipe of text read in voice v: cloned from its
// preset's reference clip (its emotion variant when emotion is set), or
// designed from its instruction when it has no preset.
func (m *Manager) voiceRecipe(v narration.ResolvedVoice, emotion, text, alignText string) clipkey.Recipe {
	if v.PresetID == "" {
		return withEffect(designRecipe(v.DesignModel, v.Instruct, v.Seed, v.Language, text, alignText), v.Effect)
	}
	cloneModel := v.CloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	return withEffect(m.clipRecipe(cloneModel, v.PresetID, emotion, v.Language, v.CloneInstruct, text, alignText), v.Effect)
}

// withEffect is r for a clip put through effect once generated.
func withEffect(r clipkey.Recipe, effect voicefx.VoiceEffect) clipkey.Recipe {
	r.Effect = voicefx.Tag(effect)
	return r
}

// silenceRecipe is an unvoiced line's recipe: its silence is shared by
// every line spelled the same, in any voice.
func silenceRecipe(text string) clipkey.Recipe {
	return clipkey.Recipe{CloneModel: "silence", Text: text, AlignText: text}
}

// generationTextModel is the clone model a paragraph's generation text is
// resolved for under voice v (store.Paragraph.ResolveGenerationText): its
// clone model, or none for a voice designed from an instruction.
func generationTextModel(v narration.ResolvedVoice) string {
	if v.PresetID == "" {
		return ""
	}
	if v.CloneModel == "" {
		return voices.DefaultCloneModel
	}
	return v.CloneModel
}

// clipTake is p's take among chapter's paragraphs (clipkey.TakeAt): which
// of several readings of its line it gets, counting up along a run of the
// same line by the same speaker in the same emotion. chapter is p's whole
// chapter in Idx order, nil to load it.
func (m *Manager) clipTake(bookID, chapterID string, p store.Paragraph, chapter []store.Paragraph, cloneModel string) int {
	if chapter == nil {
		var err error
		if chapter, err = m.store.ListParagraphsRaw(chapterID); err != nil {
			return 0
		}
	}
	// One voice reads the whole book unless it is multi-voice, and then
	// the speaker doesn't tell two lines apart.
	bySpeaker := false
	if book, err := m.store.GetBook(bookID); err == nil && book != nil {
		bySpeaker = book.MultiVoice()
	}
	return chapterTake(chapter, p.Idx, cloneModel, m.lexiconFor(bookID), bySpeaker)
}

// chapterTake is clipTake over what it looked up.
func chapterTake(chapter []store.Paragraph, idx int, cloneModel string, lexicon []pronounce.LexiconEntry, bySpeaker bool) int {
	// Idx is dense per chapter, so a paragraph sits at its own Idx.
	at := func(idx int) (store.Paragraph, bool) {
		if idx < 0 || idx >= len(chapter) || chapter[idx].Idx != idx {
			return store.Paragraph{}, false
		}
		return chapter[idx], true
	}
	speaker := func(p store.Paragraph) string {
		if !bySpeaker || p.Speaker == "Narrator" {
			return ""
		}
		return p.Speaker
	}
	return clipkey.TakeAt(idx, func(i, j int) bool {
		a, okA := at(i)
		b, okB := at(j)
		if !okA || !okB || a.Text != b.Text || a.Unvoiced() {
			return false
		}
		if speaker(a) != speaker(b) || (bySpeaker && a.SpeakerIndex != b.SpeakerIndex) {
			return false
		}
		return a.EffectiveEmotion() == b.EffectiveEmotion() &&
			a.ResolveGenerationText(cloneModel, lexicon) == b.ResolveGenerationText(cloneModel, lexicon)
	})
}

// clipRecipe is the recipe of a paragraph cloned from presetID's reference
// clip - its emotion variant when emotion is set - reading text, aligned
// against the paragraph's own plain text.
func (m *Manager) clipRecipe(cloneModel, presetID, emotion, language, cloneInstruct, text, alignText string) clipkey.Recipe {
	return clipkey.Recipe{
		CloneModel:    cloneModel,
		Reference:     voicerefs.RefIdentity(m.dataDir, presetID, emotion),
		Language:      language,
		CloneInstruct: cloneInstruct,
		Text:          text,
		AlignText:     alignText,
	}
}

// sharedClip looks key up for the paragraphs members (a clip's whole
// group, anchor first) under voiceID: the parts of a clip that is on disk,
// read from another paragraph ready on it or, when none is, from what the
// last one left behind (store.ClipOrphan). nil if there is no such clip.
// The caller holds m.clips.mu.
func (m *Manager) sharedClip(key string, members []store.Paragraph, voiceID string) []clipPart {
	shared := audiopath.SharedClipFile(m.dataDir, key)
	if _, err := os.Stat(shared); err != nil {
		// A latent-only transfer's clip is decoded when first played.
		if _, err := os.Stat(latents.Sidecar(shared)); err != nil {
			return nil
		}
	}
	users, err := m.store.ClipUsers(key)
	if err != nil {
		return nil
	}
	for _, u := range users {
		if u.State.Status != store.AudioReady || (u.ParagraphID == members[0].ID && u.VoiceID == voiceID) {
			continue
		}
		if parts := m.clipUserParts(u, len(members)); parts != nil {
			return parts
		}
	}
	if len(users) > 0 {
		return nil
	}
	// Short of the retention the sweep deletes at, so a clip is never
	// taken up as it is being swept.
	orphan, err := m.store.ClipOrphan(key)
	if err != nil || orphan == nil || time.Since(time.Unix(orphan.OrphanedAt, 0)) > clipkey.OrphanRetention-time.Hour {
		return nil
	}
	// A group's clip needs every member's part to be taken up again.
	if len(orphan.Parts) != len(members)-1 {
		return nil
	}
	parts := []clipPart{{duration: orphan.DurationSeconds, wordTimings: orphan.WordTimings}}
	for _, p := range orphan.Parts {
		parts = append(parts, clipPart{pointerSeconds: p.PointerSeconds, duration: p.DurationSeconds, wordTimings: p.WordTimings})
	}
	return parts
}

// DiscardOrphanedClips deletes the shared clips left without a paragraph
// since since - what an explicit "clear generation" calls after deleting
// the rows, so the audio it cleared is rendered anew rather than found
// again. Returns how many clips it deleted.
func (m *Manager) DiscardOrphanedClips(since time.Time) int {
	keys, err := m.store.ClipOrphansSince(since.Unix())
	if err != nil {
		log.Printf("jobs: list orphaned clips: %v", err)
		return 0
	}
	m.clips.mu.Lock()
	defer m.clips.mu.Unlock()
	removed := 0
	for _, key := range keys {
		if users, err := m.store.ClipUsers(key); err != nil || len(users) > 0 {
			continue
		}
		if err := audiopath.RemoveClip(audiopath.SharedClipFile(m.dataDir, key)); err != nil {
			log.Printf("jobs: remove cleared clip %s: %v", key, err)
			continue
		}
		removed++
	}
	if err := m.store.DeleteClipOrphans(keys); err != nil {
		log.Printf("jobs: forget orphaned clips: %v", err)
	}
	return removed
}

// AdoptStaleClips deals with the clips still sitting at a paragraph's own
// path. One kept under a voice its paragraph no longer uses - left from
// before a re-attribution or a multi-voice switch - is dropped, row and
// file: what it was rendered from isn't known any more, so it can't be
// given a content key, and it would only play again if that exact voice
// came back. One under its paragraph's current voice that AdoptSharedClips
// passed over (its key's clip was unreadable at the time) gets that pass
// again, chapter by chapter. Cheap when there is nothing to do, so it runs
// on every startup. Returns how many clips it adopted and how many it
// dropped.
func (m *Manager) AdoptStaleClips(ctx context.Context) (adopted, dropped int) {
	rows, err := m.store.KeylessClips()
	if err != nil {
		log.Printf("jobs: adopt stale clips: %v", err)
		return 0, 0
	}
	books := map[string]*store.Book{}
	current := map[string]string{} // chapter, speaker, index -> voice id
	retry := map[string]string{}   // chapter id -> book id
	var stale []store.ClipUser
	for _, u := range rows {
		if ctx.Err() != nil {
			return 0, 0
		}
		book, ok := books[u.BookID]
		if !ok {
			book, _ = m.store.GetBook(u.BookID)
			books[u.BookID] = book
		}
		p, err := m.store.GetParagraph(u.ParagraphID)
		if err != nil || p == nil || book == nil {
			continue
		}
		ch, err := m.store.GetChapterByID(p.ChapterID)
		if err != nil || ch == nil {
			continue
		}
		who := p.ChapterID + "\x00" + p.Speaker + "\x00" + strconv.Itoa(p.SpeakerIndex)
		voiceID, ok := current[who]
		if !ok {
			v, err := m.narration.ForParagraph(book, p.Speaker, p.SpeakerIndex, ch.Idx)
			if err != nil {
				continue
			}
			voiceID = v.VoiceID()
			current[who] = voiceID
		}
		if voiceID == u.VoiceID {
			retry[u.ChapterID] = u.BookID
			continue
		}
		stale = append(stale, u)
	}
	if err := m.store.DeleteClipRows(stale); err != nil {
		log.Printf("jobs: drop stale clips: %v", err)
	} else {
		for _, u := range stale {
			_ = audiopath.RemoveClip(audiopath.Resolve(audiopath.ParagraphFile(m.dataDir, u.BookID, u.ChapterID, u.VoiceID, u.Idx)))
			_ = audiopath.RemoveClip(audiopath.ParagraphFile(m.dataDir, u.BookID, u.ChapterID, u.VoiceID, u.Idx))
		}
		dropped = len(stale)
	}
	var stats AdoptStats
	voiceOf := map[string]map[string]narration.ResolvedVoice{}
	for chapterID, bookID := range retry {
		ch, err := m.store.GetChapterByID(chapterID)
		if err != nil || ch == nil || books[bookID] == nil || ctx.Err() != nil {
			continue
		}
		if voiceOf[bookID] == nil {
			voiceOf[bookID] = map[string]narration.ResolvedVoice{}
		}
		if err := m.adoptChapter(books[bookID], ch, voiceOf[bookID], &stats); err != nil {
			log.Printf("jobs: adopt shared clips in chapter %s: %v", chapterID, err)
		}
	}
	return stats.Adopted + stats.Deduplicated, dropped
}

// unsharedDroppedMarker is written once DropUnsharedClips has run.
func unsharedDroppedMarker(dataDir string) string {
	return filepath.Join(dataDir, "clips", ".unshared-dropped")
}

// DropUnsharedClips deletes, once, the clips an earlier version moved into
// the store under a key of their own (unsharedKey) instead of dropping:
// audio kept under a voice its paragraph no longer uses, whose recipe
// wasn't known. Rows, files, and the pointer rows of any merge group among
// them. Returns how many clips it dropped.
func (m *Manager) DropUnsharedClips(ctx context.Context) int {
	marker := unsharedDroppedMarker(m.dataDir)
	if _, err := os.Stat(marker); err == nil {
		return 0
	}
	rows, err := m.store.KeyedClips()
	if err != nil {
		log.Printf("jobs: drop unshared clips: %v", err)
		return 0
	}
	var unshared []store.ClipUser
	var keys []string
	for _, u := range rows {
		if u.State.ClipKey == unsharedKey(u.ParagraphID, u.VoiceID) {
			unshared = append(unshared, u)
			keys = append(keys, u.State.ClipKey)
		}
	}
	if ctx.Err() != nil {
		return 0
	}
	m.clips.mu.Lock()
	defer m.clips.mu.Unlock()
	if err := m.store.DeleteClipRows(unshared); err != nil {
		log.Printf("jobs: drop unshared clips: %v", err)
		return 0
	}
	for _, key := range keys {
		if err := audiopath.RemoveClip(audiopath.SharedClipFile(m.dataDir, key)); err != nil {
			log.Printf("jobs: drop unshared clip %s: %v", key, err)
		}
	}
	// Losing its row recorded each as orphaned; the files are gone.
	if err := m.store.DeleteClipOrphans(keys); err != nil {
		log.Printf("jobs: drop unshared clips: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
		_ = os.WriteFile(marker, nil, 0o644)
	}
	return len(unshared)
}

// unsharedKey is the key an earlier version gave a clip whose recipe
// wasn't known: its own, from which paragraph and voice it was.
func unsharedKey(paragraphID, voiceID string) string {
	return clipkey.Recipe{CloneModel: "unshared", Reference: voiceID, Text: paragraphID}.Key(0)
}

// clipUserGroup is the n paragraphs u's clip plays for: u's own, and the
// ones after it in its chapter. nil if the chapter doesn't have them.
func (m *Manager) clipUserGroup(u store.ClipUser, n int) []store.Paragraph {
	if n == 1 {
		return []store.Paragraph{{ID: u.ParagraphID, ChapterID: u.ChapterID, Idx: u.Idx}}
	}
	all, err := m.store.ListParagraphsRaw(u.ChapterID)
	if err != nil || u.Idx < 0 || u.Idx+n > len(all) || all[u.Idx].ID != u.ParagraphID {
		return nil
	}
	return all[u.Idx : u.Idx+n]
}

// clipUserParts reads the n parts of u's clip back from its rows: u's own,
// and for a merge group the pointer rows of the members after it. nil
// unless every one is ready and pointing where it should.
func (m *Manager) clipUserParts(u store.ClipUser, n int) []clipPart {
	parts := []clipPart{{duration: u.State.DurationSeconds, wordTimings: u.State.WordTimings}}
	if n == 1 {
		return parts
	}
	group := m.clipUserGroup(u, n)
	if group == nil {
		return nil
	}
	ids := make([]string, 0, n-1)
	for _, p := range group[1:] {
		ids = append(ids, p.ID)
	}
	states, err := m.store.ParagraphAudioStatuses(ids, u.VoiceID)
	if err != nil {
		return nil
	}
	for i, id := range ids {
		st := states[id]
		if st.Status != store.AudioReady || st.PointerOffset != i+1 {
			return nil
		}
		parts = append(parts, clipPart{pointerSeconds: st.PointerSeconds, duration: st.DurationSeconds, wordTimings: st.WordTimings})
	}
	return parts
}

// applyClipParts marks members (a clip's whole group, anchor first) ready
// under voiceID with parts: the anchor on the shared clip key - or, for
// key "", on a clip at its own path - and every later member as a pointer
// into the anchor's clip.
func (m *Manager) applyClipParts(members []store.Paragraph, voiceID, key string, parts []clipPart) {
	for i, p := range members {
		part := parts[i]
		var err error
		switch {
		case i > 0:
			if err = m.store.SetParagraphReadyPointer(p.ID, voiceID, i, part.pointerSeconds, part.duration); err == nil {
				err = m.store.SetParagraphWordTimings(p.ID, voiceID, part.wordTimings)
			}
		case key == "":
			if err = m.store.SetParagraphReady(p.ID, voiceID, part.duration); err == nil {
				err = m.store.SetParagraphWordTimings(p.ID, voiceID, part.wordTimings)
			}
		default:
			// The members' parts go on the anchor's row too, so the clip
			// can still be described once their pointer rows are gone.
			var later []store.ClipPart
			for _, part := range parts[1:] {
				later = append(later, store.ClipPart{PointerSeconds: part.pointerSeconds, DurationSeconds: part.duration, WordTimings: part.wordTimings})
			}
			err = m.store.SetParagraphReadyClip(p.ID, voiceID, key, part.duration, part.wordTimings, later)
		}
		if err != nil {
			log.Printf("jobs: mark %s ready: %v", p.ID, err)
		}
	}
}

// dropOwnClip removes whatever sits at a paragraph's own clip path once
// its audio is a shared clip.
func (m *Manager) dropOwnClip(bookID, chapterID, voiceID string, idx int) {
	if err := audiopath.RemoveClip(audiopath.ParagraphFile(m.dataDir, bookID, chapterID, voiceID, idx)); err != nil {
		log.Printf("jobs: remove superseded clip: %v", err)
	}
}

// useSharedClip puts t's paragraphs on t.clipKey's existing shared clip.
// false if it is no longer there to use.
func (m *Manager) useSharedClip(t *task) bool {
	members := taskMembers(t)
	m.clips.mu.Lock()
	if m.alreadyOn(t.clipKey, members[0], t.voiceID) {
		m.clips.mu.Unlock()
		return true
	}
	parts := m.sharedClip(t.clipKey, members, t.voiceID)
	if parts != nil {
		m.applyClipParts(members, t.voiceID, t.clipKey, parts)
	}
	m.clips.mu.Unlock()
	if parts == nil {
		return false
	}
	m.dropOwnClip(t.bookID, t.chapterID, t.voiceID, members[0].Idx)
	m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
	return true
}

// saveSharedClip stores audio, just rendered for members (t's whole group,
// anchor first) with parts, as t.clipKey's shared clip, and marks members
// ready on it. If the same reading was stored while this one rendered,
// members go on that instead (reused) and audio is dropped - unless t is
// fresh, in which case audio replaces the clip for every paragraph using
// it. saved is false, with nothing changed, when the clip couldn't be
// written; the caller then writes the anchor's own path.
func (m *Manager) saveSharedClip(t *task, members []store.Paragraph, audio []byte, lat *ttsproto.Latents, parts []clipPart) (saved, reused bool) {
	key := t.clipKey
	m.clips.mu.Lock()
	defer m.clips.mu.Unlock()
	defer func() {
		if saved {
			m.dropOwnClip(t.bookID, t.chapterID, t.voiceID, members[0].Idx)
		}
	}()
	if !t.fresh {
		if existing := m.sharedClip(key, members, t.voiceID); existing != nil {
			m.applyClipParts(members, t.voiceID, key, existing)
			return true, true
		}
	}
	shared := audiopath.SharedClipFile(m.dataDir, key)
	if err := audiopath.EnsureDir(shared); err != nil {
		log.Printf("jobs: shared clip dir: %v", err)
		return false, false
	}
	if err := audiopath.WriteClipLatents(shared, audio, lat); err != nil {
		log.Printf("jobs: write shared clip %s: %v", key, err)
		return false, false
	}
	m.applyClipParts(members, t.voiceID, key, parts)
	if !t.fresh {
		return true, false
	}
	// Everyone else on the clip now plays this render.
	users, err := m.store.ClipUsers(key)
	if err != nil {
		log.Printf("jobs: list users of shared clip %s: %v", key, err)
		return true, false
	}
	for _, u := range users {
		if u.ParagraphID == members[0].ID && u.VoiceID == t.voiceID {
			continue
		}
		if group := m.clipUserGroup(u, len(members)); group != nil {
			m.applyClipParts(group, u.VoiceID, key, parts)
		}
	}
	return true, false
}

// AdoptStats summarizes an AdoptSharedClips run.
type AdoptStats struct {
	// Adopted clips became shared clips; Deduplicated ones were dropped
	// for a shared clip another paragraph had already supplied.
	Adopted, Deduplicated int
	BytesFreed            int64
}

// adoptedMarker is written once every clip from before the shared store
// has been looked at, so later startups skip the pass.
func adoptedMarker(dataDir string) string {
	return filepath.Join(dataDir, "clips", ".adopted")
}

// AdoptSharedClips brings narration clips generated before the shared
// store into it, once: each ready paragraph's clip is keyed as generation
// would key it today (its current voice, emotion, pronunciation and take),
// moved to that key's place in the store and named by its row - or, where
// another paragraph already supplied that clip, deleted, its paragraph
// taking the other's duration and word timings. It can only assume a clip
// matches the paragraph's current state, which every invalidation is
// meant to guarantee. A run cut short resumes where it stopped. Meant to
// run in its own goroutine.
func (m *Manager) AdoptSharedClips(ctx context.Context) AdoptStats {
	var stats AdoptStats
	marker := adoptedMarker(m.dataDir)
	if _, err := os.Stat(marker); err == nil {
		return stats
	}
	books, err := m.store.ListBooks()
	if err != nil {
		log.Printf("jobs: adopt shared clips: %v", err)
		return stats
	}
	for i := range books {
		book := &books[i]
		n, err := m.store.CountChapters(book.ID)
		if err != nil {
			log.Printf("jobs: adopt shared clips in %s: %v", book.ID, err)
			return stats
		}
		voiceOf := map[string]narration.ResolvedVoice{}
		for idx := 0; idx < n; idx++ {
			if ctx.Err() != nil {
				return stats
			}
			ch, err := m.store.GetChapterByIdx(book.ID, idx)
			if err != nil || ch == nil {
				continue
			}
			if err := m.adoptChapter(book, ch, voiceOf, &stats); err != nil {
				log.Printf("jobs: adopt shared clips in chapter %s: %v", ch.ID, err)
				return stats
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
		_ = os.WriteFile(marker, nil, 0o644)
	}
	return stats
}

// adoptChapter is AdoptSharedClips for one chapter. voiceOf caches the
// resolved voices by chapter and speaker.
func (m *Manager) adoptChapter(book *store.Book, ch *store.Chapter, voiceOf map[string]narration.ResolvedVoice, stats *AdoptStats) error {
	all, err := m.store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return err
	}
	lexicon := m.lexiconFor(book.ID)
	voicesByParagraph := make([]narration.ResolvedVoice, len(all))
	idsByVoice := map[string][]string{}
	for i, p := range all {
		who := ch.ID + "\x00" + p.Speaker + "\x00" + strconv.Itoa(p.SpeakerIndex)
		v, ok := voiceOf[who]
		if !ok {
			if v, err = m.narration.ForParagraph(book, p.Speaker, p.SpeakerIndex, ch.Idx); err != nil {
				return err
			}
			voiceOf[who] = v
		}
		voicesByParagraph[i] = v
		idsByVoice[v.VoiceID()] = append(idsByVoice[v.VoiceID()], p.ID)
	}
	states := make(map[string]store.AudioState, len(all))
	for voiceID, ids := range idsByVoice {
		byID, err := m.store.ParagraphAudioStatuses(ids, voiceID)
		if err != nil {
			return err
		}
		for id, st := range byID {
			states[id] = st
		}
	}

	for i, p := range all {
		v, st := voicesByParagraph[i], states[p.ID]
		if st.Status != store.AudioReady || st.PointerOffset != 0 {
			continue
		}
		voiceID := v.VoiceID()
		own := audiopath.ParagraphFile(m.dataDir, book.ID, ch.ID, voiceID, p.Idx)
		if st.ClipKey != "" {
			// Named by its row already: finish a move cut short.
			m.moveAdoptedClip(own, audiopath.SharedClipFile(m.dataDir, st.ClipKey))
			continue
		}
		// Not there, or still a legacy WAV.
		if _, err := os.Stat(own); err != nil {
			continue
		}
		// A scare-quote merge group's one clip sits under its first
		// member; the rest point back at it.
		members := []store.Paragraph{p}
		for j := i + 1; j < len(all) && states[all[j].ID].PointerOffset == j-i && states[all[j].ID].Status == store.AudioReady; j++ {
			members = append(members, all[j])
		}
		textModel := generationTextModel(v)
		var key string
		switch {
		case len(members) > 1:
			key = m.voiceRecipe(v, "", mergedGenerationText(members, textModel, lexicon), mergedAlignText(members)).Key(0)
		case p.Unvoiced():
			key = silenceRecipe(p.Text).Key(0)
		default:
			emotion := ""
			if e, known := emotions.Get(p.EffectiveEmotion()); known && v.PresetID != "" && !voices.Directed(textModel) && voicerefs.VariantExists(m.dataDir, v.PresetID, e.ID) {
				emotion = e.ID
			}
			recipe := m.voiceRecipe(v, emotion, p.ResolveGenerationText(textModel, lexicon), p.Text)
			key = recipe.Key(chapterTake(all, p.Idx, textModel, lexicon, book.MultiVoice()))
		}
		m.adoptClip(key, own, members, voiceID, stats)
	}
	return nil
}

// moveAdoptedClip moves the clip at own, and its latents sidecar, to
// shared - unless shared is already there, which then stands.
func (m *Manager) moveAdoptedClip(own, shared string) bool {
	if _, err := os.Stat(own); err != nil {
		return false
	}
	if _, err := os.Stat(shared); err == nil {
		return false
	}
	if err := audiopath.EnsureDir(shared); err != nil {
		log.Printf("jobs: adopt shared clip: %v", err)
		return false
	}
	if err := os.Rename(own, shared); err != nil {
		log.Printf("jobs: adopt shared clip: %v", err)
		return false
	}
	if err := os.Rename(latents.Sidecar(own), latents.Sidecar(shared)); err != nil {
		_ = os.Remove(latents.Sidecar(shared))
	}
	return true
}

// adoptClip is AdoptSharedClips for the clip at own, which plays for
// members (anchor first) under voiceID.
func (m *Manager) adoptClip(key, own string, members []store.Paragraph, voiceID string, stats *AdoptStats) {
	m.clips.mu.Lock()
	defer m.clips.mu.Unlock()
	if parts := m.sharedClip(key, members, voiceID); parts != nil {
		var size int64
		for _, path := range []string{own, latents.Sidecar(own)} {
			if info, err := os.Stat(path); err == nil {
				size += info.Size()
			}
		}
		m.applyClipParts(members, voiceID, key, parts)
		if err := audiopath.RemoveClip(own); err != nil {
			log.Printf("jobs: adopt shared clip %s: %v", key, err)
		}
		stats.Deduplicated++
		stats.BytesFreed += size
		return
	}
	// own's file becomes the shared clip. The row names it first: until
	// the file has moved, the clip is still found at own
	// (audiopath.NarrationClipFile), and a run cut short between the two
	// finishes the move next time.
	shared := audiopath.SharedClipFile(m.dataDir, key)
	if users, err := m.store.ClipUsers(key); err != nil || len(users) > 0 {
		// Rows on this key that sharedClip couldn't read a clip back
		// from: leave both them and this clip as they are.
		return
	}
	if err := audiopath.RemoveClip(shared); err != nil {
		// A file no row names.
		log.Printf("jobs: adopt shared clip %s: %v", key, err)
		return
	}
	if err := m.store.SetParagraphClipKey(members[0].ID, voiceID, key); err != nil {
		log.Printf("jobs: record shared clip %s: %v", key, err)
		return
	}
	if !m.moveAdoptedClip(own, shared) {
		_ = m.store.SetParagraphClipKey(members[0].ID, voiceID, "")
		return
	}
	stats.Adopted++
}
