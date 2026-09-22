package store

import (
	"database/sql"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
)

// AllTables is the wildcard table name OnChange reports for a write whose
// target table couldn't be parsed out of its SQL - subscribers should treat
// it as "anything may have changed".
const AllTables = "*"

// PositionTable is the pseudo-table name OnChange reports for
// UpdatePosition's write to books instead of "books" itself. The reader
// saves position every few seconds during playback, and almost nothing
// derived from the books table depends on those three columns - reporting
// them separately lets a subscriber that doesn't care (a chapter, the
// speaker table) skip rebuilding on every save.
const PositionTable = "books.position"

// ChangeFunc receives the (deduplicated) set of tables a successful write
// touched - one Exec outside a transaction, or every Exec inside one,
// reported once on Commit.
type ChangeFunc func(tables []string)

// OnChange registers fn to be called after every successful write - see
// ChangeFunc. This is what package live's push updates hang off: wrapping
// the store's one *sql.DB here (rather than instrumenting each of its
// write methods by hand) means no write path can forget to announce
// itself. fn runs synchronously on the writing goroutine, so it must not
// block (live.Hub.Invalidate only flips a flag and signals a channel).
// Replaces any previously registered fn; nil disables notification.
func (s *Store) OnChange(fn ChangeFunc) {
	if fn == nil {
		s.db.onChange.Store(nil)
		return
	}
	s.db.onChange.Store(&fn)
}

// notifyDB is *sql.DB with Exec/Begin overridden to report which tables a
// write touched - see Store.OnChange. Reads (Query/QueryRow) pass straight
// through the embedded *sql.DB.
type notifyDB struct {
	*sql.DB
	onChange atomic.Pointer[ChangeFunc]
}

func (d *notifyDB) notify(tables []string) {
	if len(tables) == 0 {
		return
	}
	if fn := d.onChange.Load(); fn != nil {
		(*fn)(tables)
	}
}

func (d *notifyDB) Exec(query string, args ...any) (sql.Result, error) {
	res, err := d.DB.Exec(query, args...)
	if err == nil {
		if t := writtenTable(query); t != "" {
			d.notify([]string{t})
		}
	}
	return res, err
}

// execAs is Exec reporting table instead of whatever the statement's own
// target table parses as - see PositionTable.
func (d *notifyDB) execAs(table, query string, args ...any) (sql.Result, error) {
	res, err := d.DB.Exec(query, args...)
	if err == nil {
		d.notify([]string{table})
	}
	return res, err
}

func (d *notifyDB) Begin() (*notifyTx, error) {
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &notifyTx{Tx: tx, db: d}, nil
}

// notifyTx accumulates the tables its Execs touch and reports them only
// once Commit succeeds - a rolled-back transaction changed nothing.
type notifyTx struct {
	*sql.Tx
	db     *notifyDB
	tables []string
}

func (t *notifyTx) Exec(query string, args ...any) (sql.Result, error) {
	res, err := t.Tx.Exec(query, args...)
	if err == nil {
		if name := writtenTable(query); name != "" && !slices.Contains(t.tables, name) {
			t.tables = append(t.tables, name)
		}
	}
	return res, err
}

func (t *notifyTx) Commit() error {
	err := t.Tx.Commit()
	if err == nil {
		t.db.notify(t.tables)
	}
	return err
}

var writeTableRe = regexp.MustCompile(`(?is)^\s*(?:INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s+"?([a-z_][a-z0-9_]*)`)

// writtenTable extracts the target table of a single INSERT/UPDATE/DELETE
// statement, "" for a statement that doesn't write (a SELECT run through
// Exec), or AllTables for anything else it can't classify (DDL, a CTE-
// prefixed write) - erring toward over-notifying rather than silently
// missing a change.
func writtenTable(query string) string {
	if m := writeTableRe.FindStringSubmatch(query); m != nil {
		return strings.ToLower(m[1])
	}
	head := strings.ToUpper(strings.TrimSpace(query))
	if strings.HasPrefix(head, "SELECT") {
		return ""
	}
	return AllTables
}
