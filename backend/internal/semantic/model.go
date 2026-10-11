// Package semantic is search by meaning over the library's paragraphs: an
// index of one embedding per paragraph (embeddings.sqlite, beside the
// library) and a search over it. The embedding model itself runs in
// ttsworker (internal/llmworker.Embedder); this package only talks to it
// through Embedder, so it has no native dependency.
//
// A chapter's paragraphs are embedded by a jobs.KindEmbedding task - a
// phase of book preprocessing, on the GPU - and a search phrase is embedded
// on the CPU when it is asked, so searching never waits for the GPU.
package semantic

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultModelFile is the embedding model: Jina Embeddings v5 Text nano
// (retrieval), 768 dimensions. Of the models compared on this library
// (2026-10-10, 42 find-the-passage queries over 10k paragraphs of one
// series) it ranked the passage first 86% of the time and in the top 5
// always - bge-small 62%/83%, Qwen3-Embedding-0.6B 79%/88% - and embeds
// ~2,000 paragraphs a second on the GPU, ~170 on the CPU. Q8_0 gives the
// same results as F16. Its licence is CC BY-NC 4.0.
const DefaultModelFile = "jina-embeddings-v5-text-nano-retrieval-Q8_0.gguf"

// What the model wants written before a search phrase and before a
// passage. They belong to the model: another one needs its own.
const (
	queryPrefix    = "Query: "
	documentPrefix = "Document: "
)

// ModelPathFromEnv returns LECTABLE_EMBED_MODEL_PATH, or DefaultModelFile
// in ~/llm-models. Shared by cmd/server (which turns the feature off when
// the file is missing) and cmd/ttsworker (which loads it).
func ModelPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("LECTABLE_EMBED_MODEL_PATH")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return DefaultModelFile
	}
	return filepath.Join(home, "llm-models", DefaultModelFile)
}
