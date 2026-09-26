package store

import (
	"encoding/json"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

// Store is the library's data access: every read and write the rest of the
// backend makes goes through it. DuckStore is the DuckDB implementation;
// Cached wraps any Store with a read cache invalidated on writes.
//
// Every write must be reported through OnChange once it commits -
// Cached's correctness depends on it (DuckStore reports from notifyDB, so
// a new method gets this for free as long as it writes through s.db).
type Store interface {
	AddCharacterAliases(characterID string, aliases ...string) error
	AppendMusicRegions(chapterID string, regions []MusicRegionInput, lastParagraphIdx int) ([]MusicRegion, error)
	AudioRefs() (AudioRefs, error)
	BookNarrationStats(bookID, voiceID string, overrides []SpeakerVoice) (NarrationStats, error)
	BookNarrationStatsForBooks(bookVoices []BookVoice, overrides []BookSpeakerVoice) (map[string]NarrationStats, error)
	CharacterVoiceForModel(characterID, cloneModel string) (string, error)
	CharacterVoicesForModel(characterIDs []string, cloneModel string) (map[string]string, error)
	ClearBookMusicRegions(bookID string) error
	ClearBookSpeakers(bookID string) error
	ClearLegacyTags(chapterID string) ([]int, error)
	ClearMusicRegions(chapterID string) ([]MusicRegion, error)
	Close() error
	CountChapters(bookID string) (int, error)
	CountReadyAudioForSpeaker(bookID, speaker, voiceID string) (int, error)
	CreateBook(title, author, language string, coverExt string, seriesName string, seriesIndex float64, chapters []ChapterInput) (bookID string, images []CreatedImage, err error)
	CreateVoicePreset(name, instruct, refText string, seed int, speedMultiplier float64, designModel string) (VoicePreset, error)
	DeleteBook(id string) error
	DeleteBookAudio(bookID string) error
	DeleteBookmark(id string) error
	DeleteChapterAudio(chapterID string) error
	DeleteCharacter(id string) error
	DeleteParagraphAudioByVoiceID(bookID, voiceID string) ([]SpeakerAudioRef, error)
	DeleteParagraphAudioForIdxs(bookID, chapterID string, idxs []int) ([]SpeakerAudioRef, error)
	DeleteParagraphAudioForSpeaker(bookIDs []string, name string) ([]SpeakerAudioRef, error)
	DeleteVoicePreset(id string) error
	DescriptionsForBooks(bookIDs []string, characterName string, limit int) ([]DescriptionContext, error)
	ExportBookRows(bookID, characterScope string) (map[string][]json.RawMessage, error)
	ExportFingerprints(bookID, characterScope string) (book string, chapters map[int]string, err error)
	GetBook(id string) (*Book, error)
	GetChapterByID(id string) (*Chapter, error)
	GetChapterByIdx(bookID string, idx int) (*Chapter, error)
	GetCharacter(id string) (*Character, error)
	GetCharacterByName(scope, name string) (*Character, error)
	GetDefaultVoice() (DefaultVoice, error)
	GetImage(id string) (*Image, error)
	GetMusicRegion(id string) (*MusicRegion, error)
	GetParagraph(id string) (*Paragraph, error)
	GetParagraphAudioStatus(paragraphID, voiceID string) (string, error)
	GetParagraphIDByIdx(chapterID string, idx int) (string, error)
	GetParagraphSFXState(paragraphID string) (SFXState, error)
	GetVoicePreset(id string) (*VoicePreset, error)
	ImportBookRows(rows map[string][]json.RawMessage) error
	ListBookmarks(bookID string) ([]Bookmark, error)
	ListBooks() ([]Book, error)
	ListBreaks(chapterID string) ([]Break, error)
	ListChapterSummaries(bookID, voiceID string, overrides []SpeakerVoice) ([]ChapterSummary, error)
	ListChapterSummariesForBooks(bookVoices []BookVoice, overrides []BookSpeakerVoice) (map[string][]ChapterSummary, error)
	ListCharacters(scope string) ([]Character, error)
	ListImages(chapterID string) ([]Image, error)
	ListMusicRegions(chapterID string) ([]MusicRegion, error)
	ListParagraphsRaw(chapterID string) ([]Paragraph, error)
	ListParagraphsRawForBook(bookID string) ([]Paragraph, error)
	ListSeriesBooks(seriesName string) ([]Book, error)
	ListVoicePresets() ([]VoicePreset, error)
	MusicRegionCounts(bookID string) (map[string]MusicRegionCount, error)
	OnChange(fn ChangeFunc)
	ParagraphAudioStatuses(paragraphIDs []string, voiceID string) (map[string]AudioState, error)
	ParagraphSFXStates(paragraphIDs []string) (map[string]SFXState, error)
	ParagraphsDescribing(bookIDs []string, name string) ([]SpeakerAppearance, error)
	ParagraphsForSpeaker(bookIDs []string, name string) ([]SpeakerAppearance, error)
	PruneSyncTree(version string) error
	PutSyncTreeNodes(bookID, version string, nodes []SyncTreeNode) error
	QuotesForBooks(bookIDs []string, characterName string, limit int) ([]QuoteContext, error)
	ReassignCharacterSpeaker(bookID, fromName, toName string) error
	RecentChapterSpeakers(bookID string, chapterIdx, n int) ([]string, error)
	ReplaceChapterContent(chapterID, title string, blocks []BlockInput) (oldImages, newImages []CreatedImage, err error)
	ResetAllChapterMusicAudioForBook(bookID string) error
	ResetBookPass(bookID, pass string, fromIdx int) (map[string][]int, error)
	ResetChapterMusicAudio(chapterID string) ([]MusicRegion, error)
	ResetMusicRegionAudio(id string) error
	ResetParagraphAudio(id, voiceID string) error
	SearchParagraphs(bookID, query string, limit int) ([]SearchResult, error)
	SetChapterAttributed(chapterID string) error
	SetChapterDescribed(chapterID string) error
	SetChapterDirected(chapterID string) error
	SetChapterMusicScored(chapterID string) error
	SetChapterPronounced(chapterID string) error
	SetChapterScareQuoted(chapterID string) error
	SetCharacterAliases(characterID string, aliases []string) error
	SetCharacterInvalid(id string, invalid bool) error
	SetCharacterSummary(id, summary, refLine string) error
	SetCharacterVoice(characterID, cloneModel, voicePresetID string) error
	SetDefaultVoice(presetID, instruct, language string, seed int, cloneModel string) error
	SetLengthEstimate(bookID string, secPerChar float64) error
	SetMusicRegionError(id, message string) error
	SetMusicRegionGenerating(id string) error
	SetMusicRegionReady(id string, durationSeconds float64) error
	SetParagraphDescriptions(chapterID string, byDescribesIdx map[int][]string) error
	SetParagraphEmotions(chapterID string, byIdx map[int]string) error
	SetParagraphError(id, voiceID, message string) error
	SetParagraphGenerating(id, voiceID string) error
	SetParagraphPronunciation(chapterID string, byIdx map[int][]pronounce.Substitution) error
	SetParagraphReady(id, voiceID string, durationSeconds float64) error
	SetParagraphReadyPointer(id, voiceID string, pointerOffset int, pointerSeconds, durationSeconds float64) error
	SetParagraphSFXError(id, message string) error
	SetParagraphSFXGenerating(id string) error
	SetParagraphSFXPrompt(id, prompt string) error
	SetParagraphSFXReady(id string, durationSeconds float64) error
	SetParagraphSFXTriggerWord(id string, wordIdx int) error
	SetParagraphScareQuotes(chapterID string, byIdx map[int]bool) error
	SetParagraphSpeaker(paragraphID, speaker string) error
	SetParagraphSpeakers(chapterID string, bySpeakerIdx map[int]string) error
	SetParagraphWordTimings(id, voiceID, wordTimingsJSON string) error
	SpeakerLineCounts(bookID string) (map[string]int, error)
	SyncFingerprints(bookID string) (book string, chapters map[int]string, err error)
	SyncTreeNodes(bookID, version string) (map[int]SyncTreeNode, error)
	UpdateBookmarkNote(id, note string) error
	UpdatePosition(bookID string, chapterIdx, paragraphIdx int, seconds float64) error
	SyncCharacterSummariesFromPreset(presetID, instruct, refText string, instructChanged, refTextChanged bool) error
	UpdateVoice(bookID, presetID, instruct, language string, seed int, cloneModel string, characterVoiceMode CharacterVoiceMode, speechDirection, musicEnabled bool) error
	UpdateVoicePreset(id, name, instruct, refText string, seed int, speedMultiplier float64, designModel string) error
	UpdateVoicePresetSeed(id string, seed int) error
	UpsertBookmark(paragraphID, note string) (string, error)
	UpsertCharacter(scope, name string, isRole bool) (character Character, created bool, err error)
	VoicePresetGroups() (map[string]string, error)
	VoicePresetIDsForCharacter(characterID string) ([]string, error)
}

var _ Store = (*DuckStore)(nil)
