// Package instanceid gives each backend a stable identity that survives
// restarts and network-address changes, as long as its DATA_DIR persists -
// so a client (the Android app in particular, which can be pointed at
// different backends over its lifetime - see android/CLAUDE.md's "Server
// address") can tell whether it's still talking to the same backend it
// cached data from, rather than assuming so just because a book ID happens
// to match (book IDs are only unique *within* one backend's database) or
// keying its cache on the backend's URL (which breaks on every DHCP lease
// change for a LAN-hosted backend with no stable DNS).
package instanceid

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// LoadOrCreate reads dataDir's persisted instance id, or generates and
// persists a fresh one (via store.NewID, the same random-hex scheme every
// other ID in this app uses) if this is the first time this DATA_DIR has
// been started.
func LoadOrCreate(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "instance_id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	id := store.NewID()
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		return "", err
	}
	return id, nil
}
