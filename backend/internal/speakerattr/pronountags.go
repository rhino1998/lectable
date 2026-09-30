package speakerattr

import (
	"regexp"
	"strings"
)

// This file holds RefinePronounTags, attribution's last deterministic
// pass: after RefineWithSpeechTags settles lines with a named tag, it
// checks lines tagged only "he said"/"she asked" against the speaker's
// known pronouns.

// trailingPronounWord captures the pronoun of a "he said"/"she asked"
// trailing tag.
var trailingPronounWord = regexp.MustCompile(`^(?:[a-z]+ly |then )?((?i:he|she))(?: [a-z]+ly| then)? (?:` + verbAlt + `)\b`)

// nameRun finds capitalized name runs in narration, for SpeakerNameResolver.
var nameRun = regexp.MustCompile(nameRe)

// pronounLookback is how many paragraphs before a line RefinePronounTags
// searches for who "he"/"she" is.
const pronounLookback = 12

// pronounsAgree reports whether a character with stored pronouns (see
// NormalizePronouns, "he/him") can be the "he" or "she" of a tag.
// Unknown or they/them pronouns agree with nothing and contradict nothing.
func pronounsAgree(stored, word string) (agree, contradict bool) {
	word = strings.ToLower(word)
	if stored == "" || strings.HasPrefix(stored, "they/") {
		return false, false
	}
	if strings.HasPrefix(stored, word+"/") {
		return true, false
	}
	return false, true
}

// RefinePronounTags corrects speakers (paragraph Idx -> speaker, after
// RefineWithSpeechTags) whose own trailing tag is "he said"/"she asked"
// but whose known pronouns (pronouns, NormalizePronouns form) are the
// other ones: the line goes to the most recent character before it -
// named in narration (the last name in a paragraph first), or speaking -
// whose pronouns match, never a first-person narrator (who would be "I"),
// else Unknown. Untagged quotes after it in the same paragraph that had
// the same wrong speaker move with it (the tag pass had continued them
// from it). paras must be the whole chapter in order; only idx values
// present in speakers are ever changed.
//
// Qwen3.6 gave "“Steve,” he said" to the woman the man was holding.
// Replayed over ten saved attreval runs (cmd/turneval) it changed 45
// lines, none for the worse: 23 fixes, 21 to the right woman under her
// roster name (gold.json predates a rename), one wrong answer to Unknown. A second rule - an outsider answered in the
// middle of an A, B, A exchange goes to B - fixed 39 lines and broke 179:
// three-person scenes, and runs whose earlier turns were already wrong,
// break the alternation it assumes.
func RefinePronounTags(paras []ParagraphInput, speakers map[int]string, resolver *SpeakerNameResolver, pronouns map[string]string) (map[int]string, []TagChange) {
	out := make(map[int]string, len(speakers))
	for k, v := range speakers {
		out[k] = v
	}
	var changes []TagChange
	narrator := resolver.narrator()
	for start := 0; start < len(paras); {
		end := start + 1
		for end < len(paras) && paras[end].Inline {
			end++
		}
		for qi := start; qi < end; qi++ {
			if !paras[qi].IsQuote {
				continue
			}
			cur, ok := out[paras[qi].Idx]
			if !ok || isSentinel(cur) || cur == narrator {
				continue
			}
			word := pronounTag(paras, qi, end)
			if word == "" {
				continue
			}
			if _, contradict := pronounsAgree(pronouns[cur], word); !contradict {
				continue
			}
			to := recentWithPronoun(paras, qi, word, resolver, pronouns, out)
			if to == "" {
				to = "Unknown"
			}
			for qj := qi; qj < end; qj++ {
				if !paras[qj].IsQuote || out[paras[qj].Idx] != cur {
					continue
				}
				if qj != qi {
					if name, pronoun := quoteTag(paras, qj, start, end, resolver); name != "" || pronoun {
						break
					}
				}
				if _, ok := out[paras[qj].Idx]; ok {
					out[paras[qj].Idx] = to
					changes = append(changes, TagChange{Idx: paras[qj].Idx, From: cur, To: to, Reason: "pronoun tag"})
				}
			}
		}
		start = end
	}
	return out, changes
}

// quoteTag is tagFor for the quote at paras[qi] of the paragraph
// paras[start:end].
func quoteTag(paras []ParagraphInput, qi, start, end int, resolver *SpeakerNameResolver) (string, bool) {
	var prev, next *ParagraphInput
	if qi > start && !paras[qi-1].IsQuote {
		prev = &paras[qi-1]
	}
	if qi+1 < end && !paras[qi+1].IsQuote {
		next = &paras[qi+1]
	}
	return tagFor(prev, next, resolver)
}

// pronounTag returns "he"/"she" when the quote at paras[qi] has a
// trailing "he said"-style tag in its own paragraph (ending at end).
func pronounTag(paras []ParagraphInput, qi, end int) string {
	if qi+1 >= end || paras[qi+1].IsQuote {
		return ""
	}
	m := trailingPronounWord.FindStringSubmatch(strings.TrimLeft(paras[qi+1].Text, " ,.;:-—–"))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// recentWithPronoun walks back up to pronounLookback paragraphs from
// paras[qi] and returns the first character - named in narration (the
// last name in a paragraph first) or credited with a line - whose known
// pronouns are word's, never the first-person narrator.
func recentWithPronoun(paras []ParagraphInput, qi int, word string, resolver *SpeakerNameResolver, pronouns map[string]string, speakers map[int]string) string {
	fits := func(name string) bool {
		if isSentinel(name) || name == resolver.narrator() {
			return false
		}
		agree, _ := pronounsAgree(pronouns[name], word)
		return agree
	}
	for i := qi - 1; i >= 0 && i >= qi-pronounLookback; i-- {
		p := paras[i]
		if p.IsQuote {
			name := speakers[p.Idx]
			if name == "" {
				name = p.Known
			}
			if fits(name) {
				return name
			}
			continue
		}
		runs := nameRun.FindAllString(p.Text, -1)
		for j := len(runs) - 1; j >= 0; j-- {
			if name, ok := resolver.Resolve(runs[j]); ok && fits(name) {
				return name
			}
		}
	}
	return ""
}
