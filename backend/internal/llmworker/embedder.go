package llmworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

// EmbedderConfig configures an Embedder.
type EmbedderConfig struct {
	// Name tells the worker's embedders apart in logs ("gpu", "cpu").
	Name string
	// ModelPath is the embedding model's GGUF. A missing file is an error
	// on the first Embed, not at construction.
	ModelPath string
	// NoGPU keeps the model and its context off the GPU entirely;
	// otherwise every layer is offloaded.
	NoGPU bool
	// Threads is the CPU thread count (0: libllama's default).
	Threads int32
	// MaxSeqs is how many texts one evaluation holds (default 8) and
	// MaxTokens how many tokens of each are read (default 512); a longer
	// text is cut there.
	MaxSeqs   int
	MaxTokens int
	// IdleUnloadAfter frees the model once it has gone unused that long;
	// 0 keeps it.
	IdleUnloadAfter time.Duration
}

// Embedder serves an embedding model: text in, one unit-length vector
// out. The model loads on first use and unloads when idle, like Worker's.
// The worker runs two, both from the same file: one on the GPU for bulk
// work the job queue schedules (a chapter's paragraphs), and one on the
// CPU for a search phrase at any moment, whatever the GPU is doing.
type Embedder struct {
	cfg EmbedderConfig

	mu       sync.Mutex // one evaluation at a time; also guards the fields below
	model    *llamacpp.Model
	ctx      *llamacpp.Context
	lastUsed time.Time
}

// NewEmbedder creates an Embedder; nothing is loaded yet.
func NewEmbedder(cfg EmbedderConfig) *Embedder {
	if cfg.MaxSeqs <= 0 {
		cfg.MaxSeqs = 8
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 512
	}
	e := &Embedder{cfg: cfg}
	if cfg.IdleUnloadAfter > 0 {
		go e.idleLoop()
	}
	return e
}

func (e *Embedder) idleLoop() {
	for range time.Tick(15 * time.Second) {
		e.mu.Lock()
		if e.model != nil && time.Since(e.lastUsed) >= e.cfg.IdleUnloadAfter {
			log.Printf("llmworker: unloading %s embedder after %s idle", e.cfg.Name, e.cfg.IdleUnloadAfter)
			e.unloadLocked()
		}
		e.mu.Unlock()
	}
}

// Unload frees the model now; the next Embed loads it again. Waits for an
// evaluation in progress.
func (e *Embedder) Unload() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unloadLocked()
}

func (e *Embedder) unloadLocked() {
	if e.model == nil {
		return
	}
	e.ctx.Close()
	e.model.Close()
	e.model, e.ctx = nil, nil
}

func (e *Embedder) loadLocked() error {
	if e.model != nil {
		return nil
	}
	if e.cfg.ModelPath == "" {
		return errors.New("llmworker: no embedding model configured")
	}
	backendInitOnce.Do(llamacpp.BackendInit)
	start := time.Now()
	params := llamacpp.ModelParams{NGPULayers: -1}
	if e.cfg.NoGPU {
		params = llamacpp.ModelParams{NoGPU: true}
	}
	model, err := llamacpp.LoadModel(e.cfg.ModelPath, params)
	if err != nil {
		return fmt.Errorf("load embedding model: %w", err)
	}
	// An embedding model attends over a whole text at once, so the
	// physical batch has to hold every sequence of an evaluation.
	tokens := uint32(e.cfg.MaxSeqs * e.cfg.MaxTokens)
	ctx, err := model.NewContext(llamacpp.ContextParams{
		NCtx:          tokens,
		NBatch:        tokens,
		NUBatch:       tokens,
		NSeqMax:       uint32(e.cfg.MaxSeqs),
		KVUnified:     true,
		Embeddings:    true,
		NoOpOffload:   e.cfg.NoGPU,
		NThreads:      e.cfg.Threads,
		NThreadsBatch: e.cfg.Threads,
	})
	if err != nil {
		model.Close()
		return fmt.Errorf("create embedding context: %w", err)
	}
	e.model, e.ctx = model, ctx
	log.Printf("llmworker: loaded %s embedder %s in %s", e.cfg.Name, e.cfg.ModelPath, time.Since(start).Round(time.Millisecond))
	return nil
}

// Embed returns one unit-length vector per text, in order.
func (e *Embedder) Embed(texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.loadLocked(); err != nil {
		return nil, err
	}
	defer func() { e.lastUsed = time.Now() }()

	seqs := make([][]llamacpp.Token, len(texts))
	for i, text := range texts {
		toks, err := e.model.Vocab().Tokenize(text, true, false)
		if err != nil {
			return nil, fmt.Errorf("tokenize text %d: %w", i, err)
		}
		if len(toks) == 0 {
			return nil, fmt.Errorf("text %d is empty", i)
		}
		if len(toks) > e.cfg.MaxTokens {
			// Keep the closing token: with last-token pooling it is the
			// one the embedding is read from.
			toks = append(toks[:e.cfg.MaxTokens-1:e.cfg.MaxTokens-1], toks[len(toks)-1])
		}
		seqs[i] = toks
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(seqs); start += e.cfg.MaxSeqs {
		vecs, err := e.ctx.Embed(seqs[start:min(start+e.cfg.MaxSeqs, len(seqs))])
		if err != nil {
			return nil, err
		}
		for _, v := range vecs {
			out = append(out, normalize(v))
		}
	}
	return out, nil
}

// normalize scales v to unit length in place.
func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return v
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
	return v
}

// maxEmbedTexts caps one POST /embed call.
const maxEmbedTexts = 512

// HandleEmbed serves POST /embed with gpu, or cpu for a request marked
// Query (ttsproto.EmbedRequest).
func HandleEmbed(gpu, cpu *Embedder) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		var req ttsproto.EmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
		if len(req.Texts) == 0 || len(req.Texts) > maxEmbedTexts {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("texts must hold 1 to %d entries", maxEmbedTexts))
			return
		}
		e := gpu
		if req.Query {
			e = cpu
		}
		vecs, err := e.Embed(req.Texts)
		if err != nil {
			writeError(rw, http.StatusInternalServerError, err)
			return
		}
		resp := ttsproto.EmbedResponse{Dim: len(vecs[0]), Vectors: make([][]byte, len(vecs))}
		for i, v := range vecs {
			resp.Vectors[i] = ttsproto.EncodeVector(v)
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(resp)
	}
}
