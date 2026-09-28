package live

import "testing"

func TestTopicKind(t *testing.T) {
	for key, want := range map[string]string{"speakers:abc": "speakers", "jobs": "jobs", "chapter:b:3": "chapter"} {
		if got := topicKind(key); got != want {
			t.Errorf("topicKind(%q) = %q, want %q", key, got, want)
		}
	}
}
