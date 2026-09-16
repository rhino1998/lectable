package llamacpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -lllama -lggml -lggml-base -lggml-cpu
#include <stdlib.h>
#include <llama.h>
*/
import "C"

import "unsafe"

// Token is a single vocabulary entry id (llama_token).
type Token int32

// Vocab wraps a model's llama_vocab. It borrows the model's memory and is
// only valid for as long as the owning *Model is not closed.
type Vocab struct {
	handle *C.struct_llama_vocab
	model  *Model // keeps the model referenced (and alive) for as long as the Vocab is
}

// NTokens returns the size of the vocabulary.
func (v *Vocab) NTokens() int32 {
	return int32(C.llama_vocab_n_tokens(v.handle))
}

// IsEOG reports whether token is an end-of-generation marker (EOS, EOT, or
// any other model-specific stop token) -- check this in a generation loop
// instead of hardcoding a single EOS id.
func (v *Vocab) IsEOG(tok Token) bool {
	return bool(C.llama_vocab_is_eog(v.handle, C.llama_token(tok)))
}

// BOS returns the beginning-of-sentence token, or -1 if the model has none.
func (v *Vocab) BOS() Token { return Token(C.llama_vocab_bos(v.handle)) }

// EOS returns the end-of-sentence token, or -1 if the model has none.
func (v *Vocab) EOS() Token { return Token(C.llama_vocab_eos(v.handle)) }

// Tokenize converts text into tokens. addSpecial allows the tokenizer to add
// BOS/EOS as the model is configured to; parseSpecial allows control/special
// tokens embedded in text (e.g. a chat template's own markup) to be
// recognized as such rather than tokenized as plain text.
func (v *Vocab) Tokenize(text string, addSpecial, parseSpecial bool) ([]Token, error) {
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	textLen := C.int32_t(len(text))

	guess := textLen + 8
	for {
		buf := make([]C.llama_token, guess)
		n := C.llama_tokenize(
			v.handle, cText, textLen,
			(*C.llama_token)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)),
			C.bool(addSpecial), C.bool(parseSpecial),
		)
		if n >= 0 {
			out := make([]Token, n)
			for i := range out {
				out[i] = Token(buf[i])
			}
			return out, nil
		}
		if -n == guess {
			return nil, newError("llama_tokenize")
		}
		guess = -n
	}
}

// TokenToPiece renders a single token as its (partial, byte-level) text
// piece. special controls whether control/special tokens render as text or
// are suppressed.
func (v *Vocab) TokenToPiece(tok Token, special bool) string {
	buf := make([]byte, 32)
	for {
		n := C.llama_token_to_piece(
			v.handle, C.llama_token(tok),
			(*C.char)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)), 0,
			C.bool(special),
		)
		if n >= 0 {
			return string(buf[:n])
		}
		buf = make([]byte, -n)
	}
}

// Detokenize renders a sequence of tokens back to text.
// removeSpecial strips BOS/EOS the model would otherwise add; unparseSpecial
// renders control/special tokens as text instead of suppressing them.
func (v *Vocab) Detokenize(tokens []Token, removeSpecial, unparseSpecial bool) string {
	if len(tokens) == 0 {
		return ""
	}
	cTokens := make([]C.llama_token, len(tokens))
	for i, t := range tokens {
		cTokens[i] = C.llama_token(t)
	}

	guess := C.int32_t(len(tokens)) * 8
	for {
		buf := make([]byte, guess)
		n := C.llama_detokenize(
			v.handle, (*C.llama_token)(unsafe.Pointer(&cTokens[0])), C.int32_t(len(cTokens)),
			(*C.char)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)),
			C.bool(removeSpecial), C.bool(unparseSpecial),
		)
		if n >= 0 {
			return string(buf[:n])
		}
		if -n == guess {
			return ""
		}
		guess = -n
	}
}

// ChatMessage is one turn in a chat, mirroring llama_chat_message.
type ChatMessage struct {
	Role    string
	Content string
}

// ApplyChatTemplate formats messages using tmpl (a Jinja-flavored template
// string in llama.cpp's own limited dialect -- see llama.h's doc comment on
// llama_chat_apply_template for the caveats). Pass "" for tmpl to use the
// model's own built-in template (Model.ChatTemplate("")). addAssistant
// appends the token(s) that open an assistant turn, so the model continues
// straight into its reply.
func (v *Vocab) ApplyChatTemplate(tmpl string, messages []ChatMessage, addAssistant bool) (string, error) {
	var cTmpl *C.char
	if tmpl != "" {
		cTmpl = C.CString(tmpl)
		defer C.free(unsafe.Pointer(cTmpl))
	}

	cMsgs := make([]C.struct_llama_chat_message, len(messages))
	total := 0
	var freers []func()
	defer func() {
		for _, f := range freers {
			f()
		}
	}()
	for i, m := range messages {
		cRole := C.CString(m.Role)
		freers = append(freers, func() { C.free(unsafe.Pointer(cRole)) })
		cContent := C.CString(m.Content)
		freers = append(freers, func() { C.free(unsafe.Pointer(cContent)) })
		cMsgs[i] = C.struct_llama_chat_message{role: cRole, content: cContent}
		total += len(m.Role) + len(m.Content)
	}

	guess := int32(total)*2 + 32
	var msgsPtr *C.struct_llama_chat_message
	if len(cMsgs) > 0 {
		msgsPtr = &cMsgs[0]
	}
	for {
		buf := make([]byte, guess)
		n := C.llama_chat_apply_template(
			cTmpl, msgsPtr, C.size_t(len(cMsgs)), C.bool(addAssistant),
			(*C.char)(unsafe.Pointer(&buf[0])), C.int32_t(len(buf)),
		)
		if n < 0 {
			return "", newError("llama_chat_apply_template")
		}
		if int32(n) <= guess {
			return string(buf[:n]), nil
		}
		guess = int32(n)
	}
}
