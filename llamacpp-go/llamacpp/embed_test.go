package llamacpp_test

import (
	"math"
	"os"
	"testing"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

// TestEmbed needs an embedding GGUF at LLAMACPP_TEST_EMBED_MODEL (e.g.
// jina-embeddings-v5-text-nano-retrieval); skipped otherwise.
func TestEmbed(t *testing.T) {
	path := os.Getenv("LLAMACPP_TEST_EMBED_MODEL")
	if path == "" {
		t.Skip("LLAMACPP_TEST_EMBED_MODEL not set")
	}
	initBackend()
	model, err := llamacpp.LoadModel(path, llamacpp.ModelParams{NoGPU: true})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	ctx, err := model.NewContext(llamacpp.ContextParams{NCtx: 1024, NBatch: 1024, NSeqMax: 4, KVUnified: true, Embeddings: true, NoOpOffload: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	texts := []string{
		"Query: how long do kobolds live",
		"Document: Kobolds only live to fifteen or so.",
		"Document: The city smelled of salt water and rotten fish.",
	}
	seqs := make([][]llamacpp.Token, len(texts))
	for i, text := range texts {
		if seqs[i], err = model.Vocab().Tokenize(text, true, false); err != nil {
			t.Fatal(err)
		}
	}
	vecs, err := ctx.Embed(seqs)
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float32) float64 {
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		return dot / math.Sqrt(na*nb)
	}
	related, unrelated := cos(vecs[0], vecs[1]), cos(vecs[0], vecs[2])
	t.Logf("dim %d, related %.3f, unrelated %.3f", len(vecs[0]), related, unrelated)
	if related <= unrelated {
		t.Errorf("the matching passage scored %.3f, the unrelated one %.3f", related, unrelated)
	}

	// A sequence embeds the same alone as in a batch.
	alone, err := ctx.Embed(seqs[1:2])
	if err != nil {
		t.Fatal(err)
	}
	if c := cos(alone[0], vecs[1]); c < 0.999 {
		t.Errorf("alone vs batched cosine %.4f", c)
	}
}
