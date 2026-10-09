package speakerattr

import (
	"context"
	"fmt"
)

// PronunciationProbe is one ambiguous term as ProbePronunciation resolved
// it.
type PronunciationProbe struct {
	ParagraphIdx int      `json:"paragraphIdx"`
	Offset       int      `json:"offset"`
	Text         string   `json:"text"`
	Kind         string   `json:"kind"`
	Candidates   []string `json:"candidates"`
	// Rule is resolveHomographByGrammar's answer, -1 when no rule settles
	// the term.
	Rule int `json:"rule"`
	// Choice is the model's candidate.
	Choice int `json:"choice"`
	// Probs is the model's probability of each candidate (decide modes
	// only), and Mass how much of its next-token distribution the option
	// letters held.
	Probs []float64 `json:"probs,omitempty"`
	Mass  float64   `json:"mass,omitempty"`
}

// ProbePronunciation is ResolvePronunciation for benchmarking the model's
// half of it: the same batches and prompt, but every answer comes back
// with the rule's verdict beside it, and mode picks how the model answers.
//
//   - "gen": the reply is generated and parsed (what the app runs).
//   - "decide": the same reply, each letter read from the model's logits.
//   - "independent": each term in a call of its own, its letter decided.
//   - "avg": as independent, asked twice with the meanings in either
//     order and the two probabilities averaged, cancelling any lean
//     toward a letter.
//
// With all set, terms a rule settles are asked about too.
func (c *Client) ProbePronunciation(ctx context.Context, paragraphs []ParagraphInput, mode string, all bool) ([]PronunciationProbe, error) {
	decider, _ := c.llm.(llmDecider)
	if mode != "gen" && decider == nil {
		return nil, fmt.Errorf("speakerattr: this LLM backend has no decide call")
	}
	textByIdx := make(map[int]string, len(paragraphs))
	for _, p := range paragraphs {
		textByIdx[p.Idx] = p.Text
	}
	var out []PronunciationProbe
	for start := 0; start < len(paragraphs); start += pronunciationBatchParagraphs {
		batch := paragraphs[start:min(start+pronunciationBatchParagraphs, len(paragraphs))]
		byIdx := make(map[int]ParagraphInput, len(batch))
		for _, p := range batch {
			byIdx[p.Idx] = p
		}
		var terms []pronunciationTerm
		var probes []PronunciationProbe
		for _, t := range findPronunciationTerms(batch) {
			if len(t.Candidates) < 2 {
				continue
			}
			rule := -1
			if t.Kind != "" {
				p := byIdx[t.ParagraphIdx]
				if choice, ok := resolveHomographByGrammar(t.Kind, p.Text, t.Offset, t.Offset+t.Length, p.IsQuote); ok {
					rule = choice
				}
			}
			if rule >= 0 && !all {
				continue
			}
			t.ID = len(terms)
			terms = append(terms, t)
			probes = append(probes, PronunciationProbe{ParagraphIdx: t.ParagraphIdx, Offset: t.Offset, Text: t.Text, Kind: t.Kind, Candidates: t.Candidates, Rule: rule})
		}
		if len(terms) == 0 {
			continue
		}
		switch mode {
		case "gen":
			choices, err := c.pronunciationGenerate(ctx, c.pronunciationUserPrompt(textByIdx, terms, false), terms)
			if err != nil {
				return out, err
			}
			for i, t := range terms {
				probes[i].Choice = choices[t.ID]
			}
		case "decide":
			decisions, err := decider.LLMDecide(ctx, pronunciationSystemPrompt, c.pronunciationUserPrompt(textByIdx, terms, false), pronunciationSteps(terms))
			if err != nil {
				return out, err
			}
			for i, d := range decisions {
				probes[i].Choice, probes[i].Probs, probes[i].Mass = d.Choice, d.Probs, d.Mass
			}
		case "independent", "avg":
			for i, t := range terms {
				t.ID = 0
				one := []pronunciationTerm{t}
				decisions, err := decider.LLMDecide(ctx, pronunciationSystemPrompt, c.pronunciationUserPrompt(textByIdx, one, false), pronunciationSteps(one))
				if err != nil {
					return out, err
				}
				d := decisions[0]
				if mode == "avg" {
					back, err := decider.LLMDecide(ctx, pronunciationSystemPrompt, c.pronunciationUserPrompt(textByIdx, one, true), pronunciationSteps(one))
					if err != nil {
						return out, err
					}
					n := len(d.Probs)
					d.Mass = min(d.Mass, back[0].Mass)
					d.Choice = 0
					for k := range d.Probs {
						d.Probs[k] = (d.Probs[k] + back[0].Probs[n-1-k]) / 2
						if d.Probs[k] > d.Probs[d.Choice] {
							d.Choice = k
						}
					}
				}
				probes[i].Choice, probes[i].Probs, probes[i].Mass = d.Choice, d.Probs, d.Mass
			}
		default:
			return out, fmt.Errorf("speakerattr: unknown probe mode %q", mode)
		}
		out = append(out, probes...)
	}
	return out, nil
}
