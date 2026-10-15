package voices

// The model catalog: every clone model and design engine a user can pick,
// in display order, with the metadata the clients show. The source of
// truth for the frontend's model menus (cmd/apigen generates them from
// these slices); audioworker's own model tables must list exactly these
// ids (see its TestCatalogMatchesModelTables), since that package links
// audio.cpp and so can't be imported here.

// CloneModelInfo describes one clone model (store.Book.CloneModel).
type CloneModelInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// DefaultTemperature is the model's own sampling temperature, for a
	// model whose session reads a "temperature" option at all (nil: no
	// such knob - flow/diffusion models, OmniVoice).
	DefaultTemperature *float64 `json:"defaultTemperature,omitempty"`
	// Delivery is what the model reads of a line's delivery beyond its
	// words ("" is DeliveryPlain) - see CloneDelivery.
	Delivery Delivery `json:"delivery,omitempty"`
}

// Delivery is how a clone model takes a line's delivery as text.
type Delivery string

const (
	// DeliveryPlain models read words only; emotion reaches them through
	// the reference clip (internal/emotions' variants).
	DeliveryPlain Delivery = "plain"
	// DeliveryHiggsPauses is Higgs: words plus <|prosody:pause|> before
	// ellipses and dashes (internal/deliverytags).
	DeliveryHiggsPauses Delivery = "higgs-pauses"
	// DeliveryDirected models take free-form direction in the text - a
	// style prefix, point tags, scoped tags (internal/direction) - in
	// place of emotion variants: every line clones from the base clip.
	DeliveryDirected Delivery = "directed"
)

// CloneDelivery is cloneModel's Delivery.
func CloneDelivery(cloneModel string) Delivery {
	for _, m := range CloneModels {
		if m.ID == cloneModel {
			if m.Delivery == "" {
				return DeliveryPlain
			}
			return m.Delivery
		}
	}
	return DeliveryPlain
}

// Directed reports whether cloneModel takes direction as text.
func Directed(cloneModel string) bool { return CloneDelivery(cloneModel) == DeliveryDirected }

// DesignModelInfo describes one VoiceDesign engine (Preset.DesignModel).
type DesignModelInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// DefaultGuidanceScale is the engine's default guidance_scale, for an
	// engine that reads one (nil: guidance is ignored). breeze_tts's is
	// this app's own override; the rest are model defaults.
	DefaultGuidanceScale *float64 `json:"defaultGuidanceScale,omitempty"`
	// DefaultTemperature: see CloneModelInfo.DefaultTemperature.
	DefaultTemperature *float64 `json:"defaultTemperature,omitempty"`
}

func f64(v float64) *float64 { return &v }

// CloneModels is every user-selectable clone model, in display order.
var CloneModels = []CloneModelInfo{
	{ID: "audiocpp-qwen3-0.6b", Label: "Qwen3-TTS (audio.cpp)", DefaultTemperature: f64(0.9)},
	{ID: HiggsCloneModel, Label: "Higgs Audio v3 TTS 4B (audio.cpp)", DefaultTemperature: f64(0.8), Delivery: DeliveryHiggsPauses},
	{ID: InstructedCloneModel, Label: "BreezeTTS 2 (audio.cpp)", DefaultTemperature: f64(0.9)},
	{ID: "audiocpp-omnivoice", Label: "OmniVoice (audio.cpp)"},
	{ID: PocketCloneModel, Label: "PocketTTS 100M, English (audio.cpp)", DefaultTemperature: f64(0.7)},
	{ID: Drama3CloneModel, Label: "Fish Audio Drama 3 (audio.cpp)", DefaultTemperature: f64(0.7), Delivery: DeliveryDirected},
	{ID: "audiocpp-moss-local", Label: "MOSS-TTS-Local v1.5 (audio.cpp)", DefaultTemperature: f64(1.7)},
	{ID: "audiocpp-moss-nano", Label: "MOSS-TTS-Nano 100M (audio.cpp)", DefaultTemperature: f64(1.7)},
	{ID: "audiocpp-zipvoice", Label: "ZipVoice-Distill, English/Chinese (audio.cpp)"},
	{ID: "audiocpp-voxcpm2", Label: "VoxCPM2 (audio.cpp)"},
	{ID: "audiocpp-voxcpm1", Label: "VoxCPM1 0.5B (audio.cpp)"},
	{ID: "audiocpp-fireredtts3", Label: "FireRedTTS3 Instruct (audio.cpp)"},
	{ID: "audiocpp-firered", Label: "FireRedAudio (audio.cpp)"},
	{ID: "audiocpp-auk", Label: "AuK (audio.cpp, CUDA only)"},
	{ID: "audiocpp-auk-flash", Label: "AuK-Flash (audio.cpp, CUDA only)"},
}

// DesignModels is every VoiceDesign engine, in display order.
var DesignModels = []DesignModelInfo{
	{ID: DesignModelBreeze, Label: "BreezeTTS 2 (audio.cpp)", DefaultGuidanceScale: f64(4), DefaultTemperature: f64(0.9)},
	{ID: DesignModelQwen3, Label: "Qwen3-TTS VoiceDesign (audio.cpp)", DefaultTemperature: f64(0.9)},
	{ID: "omnivoice", Label: "OmniVoice (audio.cpp)"},
	{ID: "fireredtts3", Label: "FireRedTTS3 Instruct (audio.cpp)", DefaultGuidanceScale: f64(1.2)},
	{ID: "firered_audio", Label: "FireRedAudio (audio.cpp)", DefaultGuidanceScale: f64(2)},
	{ID: "auk", Label: "AuK (audio.cpp, CUDA only)", DefaultGuidanceScale: f64(2)},
	{ID: "auk_flash", Label: "AuK-Flash (audio.cpp, CUDA only)"},
	{ID: "voxcpm2", Label: "VoxCPM2 (audio.cpp)", DefaultGuidanceScale: f64(2)},
}
