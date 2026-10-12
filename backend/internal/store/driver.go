package store

import (
	"database/sql"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/rhino1998/lectable/backend/internal/namekey"
)

// driverName is go-sqlite3 plus fold(s), registered on every connection:
// Unicode case folding for case-insensitive search - SQLite's own LIKE and
// lower() only fold ASCII - and namekey(s), the form two spellings of one
// character name share (namekey.Fold). (rowhash and bit_xor are in rowhash.go.)
const driverName = "sqlite3_lectable"

func init() {
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// Memory-map the file (shared between connections through the
			// OS page cache) rather than each connection copying pages into
			// its own 2 MB default cache - the whole-book aggregates read
			// most of a book's rows.
			if _, err := conn.Exec(`PRAGMA mmap_size = 4294967296; PRAGMA cache_size = -65536; PRAGMA temp_store = MEMORY`, nil); err != nil {
				return err
			}
			if err := conn.RegisterFunc("namekey", namekey.Fold, true); err != nil {
				return err
			}
			return conn.RegisterFunc("fold", foldCase, true)
		},
	})
}

func foldCase(s string) string { return strings.ToLower(s) }
