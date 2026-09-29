package epub

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestReadZipFileSizeLimit(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, size := range map[string]int{"small.xhtml": 1024, "bomb.xhtml": maxEntrySize + 1} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(bytes.Repeat([]byte{'a'}, size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	if data, err := readZipFile(files, "small.xhtml"); err != nil || len(data) != 1024 {
		t.Fatalf("small entry: %d bytes, err %v", len(data), err)
	}
	if _, err := readZipFile(files, "bomb.xhtml"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized entry: want a limit error, got %v", err)
	}
}
