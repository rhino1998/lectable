package httpapi

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
)

// materializeLocks serializes materializing one clip path, so concurrent
// requests (an offline download plus a player, say) decode it once.
var materializeLocks sync.Map

// materializeClip makes sure clip path exists on disk, decoding it from
// its latents sidecar when only that does - a clip from a latent-only book
// transfer, decoded on first play (the worker's codec, then Opus). No
// sidecar either leaves nothing to do: serving 404s as before.
func (s *Server) materializeClip(ctx context.Context, path string) error {
	exists := func() bool {
		_, err := os.Stat(audiopath.Resolve(path))
		return err == nil
	}
	if exists() {
		return nil
	}
	side := latents.Sidecar(path)
	if _, err := os.Stat(side); err != nil {
		return nil
	}
	muAny, _ := materializeLocks.LoadOrStore(path, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if exists() {
		return nil
	}
	lat, err := latents.ReadFile(side)
	if err != nil {
		return err
	}
	wav, err := s.TTS.CodecDecode(ctx, latents.CodecFamily(lat), lat)
	if err != nil {
		return err
	}
	if err := audiopath.EnsureDir(path); err != nil {
		return err
	}
	return audiopath.WriteClipLatents(path, wav, lat)
}

// serveMaterializedClip is serveClip for a clip that may exist only as
// latents (see materializeClip).
func (s *Server) serveMaterializedClip(w http.ResponseWriter, r *http.Request, path string) {
	if err := s.materializeClip(r.Context(), path); err != nil {
		log.Printf("httpapi: decode %s from latents: %v", path, err)
		http.Error(w, "decoding audio from latents failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	serveClip(w, r, path)
}
