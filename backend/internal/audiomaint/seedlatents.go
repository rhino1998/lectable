package audiomaint

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// SeedEncoder encodes a music seed WAV as Stable Audio latents -
// ttsworker.Manager.CodecEncode with family "stable_audio_medium".
type SeedEncoder func(ctx context.Context, wav []byte) (*ttsproto.Latents, error)

// SeedMigrateStats summarizes a MigrateSeedLatents run.
type SeedMigrateStats struct {
	Converted, Failed int
	BytesBefore       int64
	BytesAfter        int64
}

// MigrateSeedLatents converts every music region's legacy seed WAV
// (<region>.seed.wav) into seed latents (<region>.seed.lat, f16) and
// removes the WAV - the form jobs now writes and musicgen seeds from.
// The latents are what today's WAV-seeded path computes from the same
// file anyway (the Stable Audio encoder), so continuations behave the
// same; only storage changes (~1.76 MB -> ~55 KB per seed).
//
// Each encode loads the worker's codec engine, which evicts any
// generation model, so this only runs once idle() has held for
// quietPeriod (a music region's chained calls leave brief gaps that
// mustn't count), then converts back to back until anything else uses
// the worker. A failed encode leaves the WAV (still a valid seed) and
// moves on.
func MigrateSeedLatents(ctx context.Context, dataDir string, encode SeedEncoder, idle func() bool) SeedMigrateStats {
	var stats SeedMigrateStats
	start := time.Now()
	seeds, _ := filepath.Glob(filepath.Join(dataDir, "audio", "*", "*", "music", "*.seed.wav"))
	for _, wavPath := range seeds {
		latPath := strings.TrimSuffix(wavPath, ".wav") + latents.Ext
		if _, err := os.Stat(latPath); err == nil {
			_ = os.Remove(wavPath) // already converted; the WAV is a leftover
			continue
		}
		if !idle() && !waitQuiet(ctx, idle) {
			return stats
		}
		if ctx.Err() != nil {
			return stats
		}
		data, err := os.ReadFile(wavPath)
		if err != nil {
			continue // removed meanwhile (region regenerated or deleted)
		}
		lat, err := encode(ctx, data)
		if err == nil {
			err = latents.WriteFile(latPath, lat, "f16")
		}
		if err != nil {
			stats.Failed++
			log.Printf("audiomaint: convert music seed %s to latents: %v", wavPath, err)
			continue
		}
		// The region may have been regenerated (writing fresh latents and
		// removing the WAV) while this one encoded; only drop the WAV if
		// it's still the file we converted.
		if st, err := os.Stat(wavPath); err == nil && st.Size() == int64(len(data)) {
			_ = os.Remove(wavPath)
		}
		stats.Converted++
		stats.BytesBefore += int64(len(data))
		if st, err := os.Stat(latPath); err == nil {
			stats.BytesAfter += st.Size()
		}
	}
	if stats.Converted+stats.Failed > 0 {
		log.Printf("audiomaint: music seeds -> latents: %d converted, %d failed, %s -> %s in %s",
			stats.Converted, stats.Failed, mb(stats.BytesBefore), mb(stats.BytesAfter), time.Since(start).Round(time.Second))
	}
	return stats
}

// quietPeriod is how long the worker must stay idle before migration
// takes it.
const quietPeriod = 20 * time.Second

// waitQuiet blocks until idle() has held continuously for quietPeriod;
// false if ctx ended first.
func waitQuiet(ctx context.Context, idle func() bool) bool {
	var since time.Time
	for {
		if idle() {
			if since.IsZero() {
				since = time.Now()
			} else if time.Since(since) >= quietPeriod {
				return true
			}
		} else {
			since = time.Time{}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}
