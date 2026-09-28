package llmworker

import "testing"

func TestStripThinking(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain answer", "plain answer"},
		{"  padded  \n", "padded"},
		{"<think>reasoning</think>\n\nanswer", "answer"},
		{"<thinking>long\ntrace</thinking> answer", "answer"},
		{"<think></think>answer", "answer"},
		// An unterminated trace (hit max tokens mid-thought) has no answer.
		{"<think>never finished", ""},
		// Only a *leading* block is a thinking trace.
		{"answer <think>x</think>", "answer <think>x</think>"},
	}
	for _, c := range cases {
		if got := stripThinking(c.in); got != c.want {
			t.Errorf("stripThinking(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
