package store

import (
	"database/sql"
	"log"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
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

// OnChange adds fn to the listeners called after every successful write -
// see ChangeFunc. This is what package live's push updates and Cached's
// invalidation hang off: wrapping the store's one *sql.DB here (rather
// than instrumenting each of its write methods by hand) means no write
// path can forget to announce itself. fn runs synchronously on the
// writing goroutine before the write method returns, so it must not block
// (live.Hub.Invalidate only flips a flag and signals a channel). nil is
// ignored.
func (s *DuckStore) OnChange(fn ChangeFunc) {
	if fn == nil {
		return
	}
	for {
		old := s.db.onChange.Load()
		var next []ChangeFunc
		if old != nil {
			next = append(next, *old...)
		}
		next = append(next, fn)
		if s.db.onChange.CompareAndSwap(old, &next) {
			return
		}
	}
}

// slowCallThreshold is the duration at or above which a DB call is logged
// (DB_SLOW_LOG_MS, default 100ms; 0 logs every call, negative disables).
// With the store's single connection (SetMaxOpenConns(1)) a call's time
// includes waiting for that connection, so a slow line usually means
// "queued behind other DB work" rather than "this query is slow" - the
// transaction lines (time a Begin held the connection) show who held it.
var slowCallThreshold = func() time.Duration {
	if v := os.Getenv("DB_SLOW_LOG_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
		log.Printf("store: DB_SLOW_LOG_MS=%q is not a valid int, using 100", v)
	}
	return 100 * time.Millisecond
}()

// logCallTiming records op's duration (dbCallDuration) and logs it if it
// took at least slowCallThreshold, naming the Store method that made the
// call.
func logCallTiming(op string, start time.Time) {
	d := time.Since(start)
	dbCallDuration.WithLabelValues(metricOp(op)).Observe(d.Seconds())
	if slowCallThreshold < 0 || d < slowCallThreshold {
		return
	}
	log.Printf("store: timing %s %s took %s", callerName(), op, d.Round(time.Millisecond))
}

// callerName returns the first function outside this file on the stack -
// the Store method behind a DB call.
func callerName() string {
	pcs := make([]uintptr, 8)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasSuffix(f.File, "/notify.go") {
			return f.Function[strings.LastIndex(f.Function, "/")+1:]
		}
		if !more {
			return "?"
		}
	}
}

// notifyDB is *sql.DB with Exec/Begin overridden to report which tables a
// write touched - see Store.OnChange - and every call timed (see
// logCallTiming).
type notifyDB struct {
	*sql.DB
	onChange atomic.Pointer[[]ChangeFunc] // copy-on-write listener list
}

func (d *notifyDB) Query(query string, args ...any) (*sql.Rows, error) {
	defer logCallTiming("query", time.Now())
	return d.DB.Query(query, args...)
}

func (d *notifyDB) QueryRow(query string, args ...any) *sql.Row {
	defer logCallTiming("queryrow", time.Now())
	return d.DB.QueryRow(query, args...)
}

func (d *notifyDB) notify(tables []string) {
	if len(tables) == 0 {
		return
	}
	if fns := d.onChange.Load(); fns != nil {
		for _, fn := range *fns {
			fn(tables)
		}
	}
}

func (d *notifyDB) Exec(query string, args ...any) (sql.Result, error) {
	defer logCallTiming("exec", time.Now())
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
	defer logCallTiming("exec", time.Now())
	res, err := d.DB.Exec(query, args...)
	if err == nil {
		d.notify([]string{table})
	}
	return res, err
}

func (d *notifyDB) Begin() (*notifyTx, error) {
	start := time.Now()
	tx, err := d.DB.Begin()
	logCallTiming("begin (connection wait)", start)
	if err != nil {
		return nil, err
	}
	return &notifyTx{Tx: tx, db: d, begun: time.Now()}, nil
}

// notifyTx accumulates the tables its Execs touch and reports them only
// once Commit succeeds - a rolled-back transaction changed nothing.
type notifyTx struct {
	*sql.Tx
	db     *notifyDB
	tables []string
	begun  time.Time
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
	defer logCallTiming("transaction held (commit)", t.begun)
	err := t.Tx.Commit()
	if err == nil {
		t.db.notify(t.tables)
	}
	return err
}

// Rollback times a transaction that didn't commit. The usual deferred
// Rollback after a successful Commit is a no-op (sql.ErrTxDone) and isn't
// logged again.
func (t *notifyTx) Rollback() error {
	err := t.Tx.Rollback()
	if err == nil {
		logCallTiming("transaction held (rollback)", t.begun)
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
