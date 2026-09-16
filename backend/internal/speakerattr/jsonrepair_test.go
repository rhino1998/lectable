package speakerattr

import "testing"

// textItem is a test-only fixture shaped like parseTaggedText's old JSON
// wire format ({"idx", "text"}) - production code no longer has any caller
// with a "text" field (see unmarshalRepairedArray's own doc comment for
// why direction/sfx tagging moved off JSON entirely), but it's still a
// realistic, easy-to-read stand-in for exercising unmarshalRepairedArray's
// generic repair/re-validate behavior below without coupling these tests
// to parseAttributions' own idx/speaker/role shape.
type textItem struct {
	Idx  int    `json:"idx"`
	Text string `json:"text"`
}

// TestUnmarshalRepairedArrayDroppedClosingQuoteStillWorks exercises
// repairDroppedClosingQuoteInArray, unmarshalRepairedArray's one remaining
// repair - a real, confirmed-in-production malformed-JSON pattern (see its
// own doc comment).
func TestUnmarshalRepairedArrayDroppedClosingQuoteStillWorks(t *testing.T) {
	raw := `[{"idx": 104, "text": "Go on, get out of here.”}, {"idx": 105, "text": "Thank you, Jake,”}]`

	var items []textItem
	if err := unmarshalRepairedArray(raw, &items); err != nil {
		t.Fatalf("unmarshalRepairedArray: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(items), items)
	}
	if items[0].Text != "Go on, get out of here.”" || items[1].Text != "Thank you, Jake,”" {
		t.Fatalf("unexpected repaired text: %+v", items)
	}
}

// TestUnmarshalRepairedArrayGenuinelyBroken confirms a response that isn't
// either known pattern still fails, with the original error, rather than
// the repair attempt masking a real problem.
func TestUnmarshalRepairedArrayGenuinelyBroken(t *testing.T) {
	raw := `[{"idx": 1, "text": }]`

	var items []textItem
	if err := unmarshalRepairedArray(raw, &items); err == nil {
		t.Fatalf("expected an error for genuinely malformed JSON, got items: %+v", items)
	}
}
