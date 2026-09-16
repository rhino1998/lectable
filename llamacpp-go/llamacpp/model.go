package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

import "unsafe"

// LoadMode mirrors enum llama_load_mode -- how the model file is read off
// disk (mmap, mlock, direct I/O, or some combination). The zero value is
// LoadModeAuto.
type LoadMode int32

const (
	LoadModeAuto      LoadMode = C.LLAMA_LOAD_MODE_AUTO
	LoadModeNone      LoadMode = C.LLAMA_LOAD_MODE_NONE
	LoadModeMmap      LoadMode = C.LLAMA_LOAD_MODE_MMAP
	LoadModeMlock     LoadMode = C.LLAMA_LOAD_MODE_MLOCK
	LoadModeMmapMlock LoadMode = C.LLAMA_LOAD_MODE_MMAP_MLOCK
	LoadModeDirectIO  LoadMode = C.LLAMA_LOAD_MODE_DIRECT_IO
)

// ModelParams configures LoadModel, mirroring the handful of
// llama_model_params fields a single-GPU-or-CPU caller typically needs.
// Everything else keeps llama_model_default_params()'s value.
type ModelParams struct {
	// NGPULayers is the number of transformer layers to offload to GPU.
	// 0 (the zero value) means CPU-only; a negative value means "all
	// layers" -- pass -1 to offload everything a GPU backend can take.
	NGPULayers int32

	// LoadMode controls how the model file is read (mmap/mlock/direct
	// I/O). LoadModeAuto (the zero value) lets libllama decide.
	LoadMode LoadMode

	// VocabOnly loads just the vocabulary, not the tensor weights --
	// enough to tokenize/detokenize or apply a chat template without
	// paying for a full model load.
	VocabOnly bool
}

func (p ModelParams) cParams() C.struct_llama_model_params {
	c := C.llama_model_default_params()
	c.n_gpu_layers = C.int32_t(p.NGPULayers)
	c.load_mode = C.enum_llama_load_mode(p.LoadMode)
	c.vocab_only = C.bool(p.VocabOnly)
	return c
}

// Model wraps a loaded llama_model.
type Model struct {
	handle *C.struct_llama_model
	vocab  *Vocab
}

// LoadModel loads a GGUF model from path. The returned *Model must be
// closed (typically via defer) when no longer needed; any Context created
// from it must be closed first.
func LoadModel(path string, params ModelParams) (*Model, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.llama_model_load_from_file(cPath, params.cParams())
	if handle == nil {
		return nil, newError("llama_model_load_from_file")
	}

	m := &Model{handle: handle}
	m.vocab = &Vocab{handle: C.llama_model_get_vocab(handle), model: m}
	return m, nil
}

// Close frees the underlying llama_model. Safe to call more than once, and
// on a nil *Model. Any Context created from this Model must already be
// closed.
func (m *Model) Close() {
	if m == nil || m.handle == nil {
		return
	}
	C.llama_model_free(m.handle)
	m.handle = nil
}

// Vocab returns this model's vocabulary. The returned *Vocab is valid for
// as long as the Model is not closed.
func (m *Model) Vocab() *Vocab {
	return m.vocab
}

// NCtxTrain returns the context size the model was trained with.
func (m *Model) NCtxTrain() int32 {
	return int32(C.llama_model_n_ctx_train(m.handle))
}

// NEmbd returns the model's embedding dimension.
func (m *Model) NEmbd() int32 {
	return int32(C.llama_model_n_embd(m.handle))
}

// NParams returns the total number of parameters in the model.
func (m *Model) NParams() uint64 {
	return uint64(C.llama_model_n_params(m.handle))
}

// Description returns a short human-readable description of the model
// (architecture, size, quantization -- the same line llama.cpp's CLI tools
// print at load time).
func (m *Model) Description() string {
	buf := make([]byte, 256)
	n := C.llama_model_desc(m.handle, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	if n < 0 {
		return ""
	}
	return string(buf[:n])
}

// ChatTemplate returns the model's built-in chat template (from GGUF
// metadata), or "" if it doesn't have one. Pass the result to
// Vocab.ApplyChatTemplate. name selects a named alternate template (e.g.
// "tool_use"); pass "" for the default template.
func (m *Model) ChatTemplate(name string) string {
	var cName *C.char
	if name != "" {
		cName = C.CString(name)
		defer C.free(unsafe.Pointer(cName))
	}
	tmpl := C.llama_model_chat_template(m.handle, cName)
	if tmpl == nil {
		return ""
	}
	return C.GoString(tmpl)
}
