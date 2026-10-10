package patreon

import "testing"

func TestTiptapHTML(t *testing.T) {
	got, err := tiptapHTML(`{"type":"doc","content":[
		{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"Part <One>"}]},
		{"type":"paragraph","content":[
			{"type":"text","text":"Plain, "},
			{"type":"text","marks":[{"type":"bold"},{"type":"italic"}],"text":"loud"},
			{"type":"hardBreak"},
			{"type":"text","marks":[{"type":"link","attrs":{"href":"https://example.test/?a=1&b=2"}}],"text":"a link"}]},
		{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item"}]}]}]},
		{"type":"somethingNew","content":[{"type":"paragraph","content":[{"type":"text","text":"kept"}]}]},
		{"type":"image","attrs":{"src":"https://example.test/a.png"}},
		{"type":"horizontalRule"}]}`)
	want := `<h3>Part &lt;One&gt;</h3>` +
		`<p>Plain, <strong><em>loud</em></strong><br/><a href="https://example.test/?a=1&amp;b=2">a link</a></p>` +
		`<ul><li><p>item</p></li></ul>` +
		`<p>kept</p>` +
		`<p><img src="https://example.test/a.png" alt=""/></p>` +
		`<hr/>`
	if err != nil || got != want {
		t.Errorf("tiptapHTML = %v\n got %s\nwant %s", err, got, want)
	}
	if _, err := tiptapHTML(`<p>not json</p>`); err == nil {
		t.Error("markup read as a document")
	}
}
