package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/websource"
)

// An archive.org item (archive.org/details/<item>) is a folder of files.
// One with a freely downloadable epub can be imported by its link: the
// epub is downloaded and imported as if it had been uploaded - an ordinary
// book, with no source to check for more chapters - except that, as for
// any import by link, nothing is generated for it yet. Items that are
// only lent (their epub encrypted) have nothing to download.

// maxArchiveEpub bounds a downloaded epub.
const maxArchiveEpub = 100 << 20

// archiveOrgClient downloads from archive.org at its own pace.
var archiveOrgClient = &websource.Client{}

// archiveOrgBase is replaced in tests.
var archiveOrgBase = "https://archive.org"

type archiveItem struct {
	id string
	// file is the file the link named, "" to pick the item's epub.
	file string
}

// archiveOrgItem recognises a link to an archive.org item
// (/details/<item>, or /download/<item>/<file> for one file of it).
func archiveOrgItem(raw string) (archiveItem, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return archiveItem{}, false
	}
	if host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."); host != "archive.org" {
		return archiveItem{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[1] == "" || (parts[0] != "details" && parts[0] != "download") {
		return archiveItem{}, false
	}
	item := archiveItem{id: parts[1]}
	if parts[0] == "download" && len(parts) > 2 {
		item.file = strings.Join(parts[2:], "/")
	}
	return item, true
}

// importArchiveOrgItem downloads an item's epub and imports it as a book.
func (s *Server) importArchiveOrgItem(ctx context.Context, item archiveItem) (bookSummaryDTO, error) {
	body, _, _, err := archiveOrgClient.Get(ctx, archiveOrgBase+"/metadata/"+url.PathEscape(item.id), 32<<20)
	if errors.Is(err, websource.ErrNotFound) {
		return bookSummaryDTO{}, httpError(http.StatusNotFound, "archive.org has no item \""+item.id+"\"")
	}
	if err != nil {
		return bookSummaryDTO{}, httpError(http.StatusBadGateway, "could not read from archive.org: "+err.Error())
	}
	var meta struct {
		Metadata struct {
			Title json.RawMessage `json:"title"`
		} `json:"metadata"`
		Files []struct {
			Name    string `json:"name"`
			Format  string `json:"format"`
			Private string `json:"private"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		// Its "temporarily offline" page, most likely.
		return bookSummaryDTO{}, httpError(http.StatusBadGateway, "archive.org didn't answer with the item's details - it may be down; try again later")
	}
	if len(meta.Files) == 0 {
		// An item that doesn't exist answers with an empty object.
		return bookSummaryDTO{}, httpError(http.StatusNotFound, "archive.org has no item \""+item.id+"\"")
	}
	name := ""
	for _, f := range meta.Files {
		isEpub := strings.HasSuffix(strings.ToLower(f.Name), ".epub")
		locked := f.Private == "true" || strings.Contains(strings.ToLower(f.Format), "encrypted") || strings.HasSuffix(strings.ToLower(f.Name), "_lcp.epub")
		switch {
		case item.file != "" && f.Name == item.file && isEpub && !locked:
			name = f.Name
		case item.file == "" && name == "" && isEpub && !locked:
			name = f.Name
		}
	}
	if name == "" {
		if item.file != "" {
			return bookSummaryDTO{}, httpError(http.StatusUnprocessableEntity, "that file of the archive.org item isn't an epub that can be downloaded")
		}
		return bookSummaryDTO{}, httpError(http.StatusUnprocessableEntity, "that archive.org item has no epub that can be downloaded (items that are only lent can't be imported)")
	}
	data, _, _, err := archiveOrgClient.Get(ctx, archiveOrgBase+"/download/"+url.PathEscape(item.id)+"/"+escapePath(name), maxArchiveEpub)
	if err != nil {
		return bookSummaryDTO{}, httpError(http.StatusBadGateway, fmt.Sprintf("could not download %s from archive.org: %v", name, err))
	}
	var title string
	_ = json.Unmarshal(meta.Metadata.Title, &title)
	if title == "" {
		title = strings.TrimSuffix(name, ".epub")
	}
	return s.importEpub(bytes.NewReader(data), int64(len(data)), title, "", false)
}

// escapePath escapes a file name that may hold directories.
func escapePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
