// Package lac is a pure-Go lossless codec for 16-bit PCM audio.
//
// It uses the same design as Monkey's Audio and OptimFROG: a fixed
// pre-emphasis filter, a cascade of backward-adaptive predictors
// (normalized LMS in float64, Monkey's-Audio-style sign-sign LMS in
// integers), an optional adaptive mix of the stage predictions, and an
// adaptive binary range coder for the residual. Every predictor adapts
// only from samples the decoder has already reconstructed, so a stream
// carries no per-block side information, just a small header.
//
// On lectable's own audio it is 5-11% smaller than FLAC -8 (TTS speech
// ~10%, music ~8%, ambience ~3%) and decodes at roughly 125x real time
// for mono 24 kHz speech (PresetSpeech) and 150x for stereo 44.1 kHz
// music (PresetMusic) on one core.
//
// # Determinism
//
// Decoding must reproduce the encoder's predictions bit for bit on every
// platform, so the float code follows three rules:
//
//   - every product is wrapped in an explicit float64 conversion, which
//     the Go spec defines as forcing a rounding step: without it the
//     compiler may fuse x*y+z into one FMA instruction (it does on arm64),
//     which rounds differently;
//   - sums run in a fixed order, and the running input power used for
//     normalization is an exact integer;
//   - float-to-int conversion goes through clampRound, which never
//     converts a NaN or out-of-range value (Go leaves those
//     implementation-defined).
//
// TestGolden pins the exact bytes of an encoding, so any change to the
// arithmetic fails loudly instead of silently breaking existing files.
// A format change must bump the version byte and keep decoding old ones.
//
// # Format
//
//	"LAC" version:u8 channels:u8 sampleRate:u32 frames:u64 crc32:u32
//	preEmph:u8 mixMu:f64 nStages:u8 { kind:u8 order:u16 mu:f64 shift:u8 }...
//	range-coded residuals, sample-interleaved
//
// All integers are little-endian; crc32 (IEEE) covers the decoded PCM
// as little-endian int16s. The stage list is stored in each stream, so
// presets can change without breaking old files.
package lac
