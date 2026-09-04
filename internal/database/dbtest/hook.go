// Package dbtest is test support for the shared SQLite handle: it opens a
// database through a database/sql driver wrapper that reports every
// statement and transaction boundary to a hook BEFORE it reaches the driver.
// A test can therefore interleave a concurrent writer between the statements
// of a read deterministically — no sleeps, no scheduler luck, and no seam in
// production code.
//
// The hook runs synchronously on the goroutine executing the statement, while
// that goroutine holds the pool's only connection (OpenHooked mirrors the
// production limit of one open connection). A hook that needs to write must
// therefore use a SEPARATE handle on the same file (a plain sql.Open), never
// the hooked handle, which would wait for its own connection. One exception:
// when a transaction's context is cancelled, database/sql rolls it back on
// its own goroutine and the "rollback" event arrives there, so a hook must be
// safe to call from another goroutine (the rigs guard their state with a
// mutex).
package dbtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	_ "modernc.org/sqlite"
)

// Event is one statement or transaction boundary about to run on the hooked
// connection. Kind is one of "begin", "commit", "rollback", "query", "exec"
// or "prepare"; SQL is the statement text for the last three and empty for
// the transaction boundaries. Ctx is the context the driver received for a
// begin, prepare, exec or query (nil for commit and rollback), so a test can
// check its lineage — a value carried by the caller's context — and not only
// whether it can be cancelled; Cancellable is Ctx.Done() != nil, false for
// context.Background(), which is what a statement issued without the
// caller's context runs under.
type Event struct {
	Kind        string
	SQL         string
	Ctx         context.Context
	Cancellable bool
}

// Hook observes every Event before it reaches the driver.
type Hook func(Event)

var (
	resolveOnce sync.Once
	underlying  driver.Driver // the registered modernc.org/sqlite driver
)

// OpenHooked opens the SQLite file at path through the hooked driver with the
// production connection limit (one open connection) and returns the shared
// handle type the repositories use. The hook travels with the handle's own
// connector — nothing is registered globally — so closing the handle releases
// everything. The caller owns the handle and closes it.
func OpenHooked(path string, hook Hook) *database.DB {
	resolveOnce.Do(func() {
		probe, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic(fmt.Sprintf("dbtest: resolve sqlite driver: %v", err))
		}
		underlying = probe.Driver()
		_ = probe.Close()
	})
	sqlDB := sql.OpenDB(hookConnector{path: path, hook: hook})
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

// hookConnector opens connections to one file and wraps each in a hookConn.
type hookConnector struct {
	path string
	hook Hook
}

func (c hookConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := underlying.Open(c.path)
	if err != nil {
		return nil, err
	}
	cc, ok := conn.(contextConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("dbtest: %T lacks the context-aware statement interfaces", conn)
	}
	return &hookConn{contextConn: cc, hook: c.hook}, nil
}

func (hookConnector) Driver() driver.Driver { return underlying }

// contextConn is the shape of connection the wrapper reports on: the
// context-aware statement and transaction interfaces database/sql prefers,
// which modernc.org/sqlite implements. Connect refuses any other connection.
type contextConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
}

// hookConn reports each statement or transaction boundary, then forwards it
// to the wrapped connection. BeginTx, PrepareContext, ExecContext and
// QueryContext are always forwarded; Ping, ResetSession and IsValid are
// forwarded when the wrapped connection implements driver.Pinger,
// driver.SessionResetter and driver.Validator (modernc.org/sqlite does) and
// otherwise answer as database/sql would without them. Prepare, Begin and
// Close are the embedded connection's.
type hookConn struct {
	contextConn
	hook Hook
}

func (c *hookConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.hook(Event{Kind: "begin", Ctx: ctx, Cancellable: ctx.Done() != nil})
	t, err := c.contextConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &hookTx{Tx: t, hook: c.hook}, nil
}

func (c *hookConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.hook(Event{Kind: "prepare", SQL: query, Ctx: ctx, Cancellable: ctx.Done() != nil})
	return c.contextConn.PrepareContext(ctx, query)
}

func (c *hookConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.hook(Event{Kind: "exec", SQL: query, Ctx: ctx, Cancellable: ctx.Done() != nil})
	return c.contextConn.ExecContext(ctx, query, args)
}

func (c *hookConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.hook(Event{Kind: "query", SQL: query, Ctx: ctx, Cancellable: ctx.Done() != nil})
	return c.contextConn.QueryContext(ctx, query, args)
}

func (c *hookConn) Ping(ctx context.Context) error {
	if p, ok := c.contextConn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *hookConn) ResetSession(ctx context.Context) error {
	if r, ok := c.contextConn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *hookConn) IsValid() bool {
	if v, ok := c.contextConn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

type hookTx struct {
	driver.Tx
	hook Hook
}

func (t *hookTx) Commit() error {
	t.hook(Event{Kind: "commit"})
	return t.Tx.Commit()
}

func (t *hookTx) Rollback() error {
	t.hook(Event{Kind: "rollback"})
	return t.Tx.Rollback()
}
