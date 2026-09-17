# Database integration

There is **nothing** DB-related in the framework: no repository interface, no ORM wrapper, no `WithDatabase`. This page is not about an API — it is about **where things go and how they fail** when you wire `database/sql` or a third-party DB library in. The rule of thumb comes first, three complete examples come last, and transactions sit in between.

## The rule of thumb

**A connection pool is a process-level resource; a transaction is a request-level resource.** The pool belongs on the assembly surface (`app.Root()`), the transaction belongs in the request scope (`c.Kernel()`). Getting this wrong does not fail immediately — it shows up later as "it got slow once concurrency arrived" or "why do we have so many pools".

## The pool goes on the assembly surface

```go
var dbKey = kernel.NewServiceKey[*sql.DB]("app.db")

func main() {
	pool, err := sql.Open("pgx", dsn)     // or sqlx.Connect / gorm.Open
	if err != nil {
		panic(err)
	}
	pool.SetMaxOpenConns(16)              // pool sizing has nothing to do with requests — decide it at assembly time
	pool.SetMaxIdleConns(16)

	app := web.New()
	if _, err := kernel.Provide(app.Root(), dbKey, pool); err != nil {
		panic(err)
	}
	app.OnShutdown(func(context.Context) error { return pool.Close() })

	app.GET("/orders", listOrders)
	app.Run(":8080")
}

func listOrders(c *web.Ctx) error {
	pool := c.MustService(dbKey)          // a global binding: readable from any scope
	rows, err := pool.QueryContext(c.Context(), `SELECT id, sku FROM orders`)
	...
}
```

**Do not provide the pool into the request scope**: `c.Kernel()` is a request scope and dies with the request — providing a pool there means one pool per request.

**Who closes the pool**: `kernel.Provide` only registers a binding, it does **not** close `io.Closer` values for you (disposing the binding only removes the service from the store). The pool's lifetime belongs to whoever assembled it, so closing it must be stated explicitly.

### When the host already has a pool

`web.WithRoot(k)` attaches to the host's kernel tree; provide the same key on that tree and both components read the same pool, neither owning the other:

```go
k := kernel.New()                      // the host's root (a process that already has its own assembly)
kernel.Provide(k, dbKey, hostPool)     // the host registers its own pool

app := web.New(web.WithRoot(k))        // the framework joins the same tree
// from here app.Root() == k, and c.MustService(dbKey) in a handler returns hostPool
```

Note that `Run` / `Serve` **cascade-dispose** that tree on return; closing the pool is still the job of whoever owns it (the host, in this case).

### Closing the pool is step ③ of the shutdown chain

```
srv.Shutdown (drains in-flight requests) → OnShutdown callback → root.Dispose → sink flush
```

Put the pool in `OnShutdown`: in-flight requests are done by then (nobody borrows connections anymore) and `root.Dispose` has not happened yet. The full chain and its budget are in [Assembly and running](/en/guide/assembly).

## Request-scoped transactions

### Middleware plus `kernel.Local()`

Open the transaction, bind it into **this request's** scope, and let handlers just use it:

```go
var txKey = kernel.NewServiceKey[*sql.Tx]("app.tx")

func TxMiddleware(c *web.Ctx, next web.Handler) error {
	pool := c.MustService(dbKey)
	tx, err := pool.BeginTx(c.Context(), nil)      // the request ctx — why, see "Failure modes"
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()           // covers panics and early returns; ErrTxDone after a commit, ignore it
	if _, err := kernel.Provide(c.Kernel(), txKey, tx, kernel.Local()); err != nil {
		return err
	}
	if err := next(c); err != nil {
		return err                                 // hand it back to the central error mapper for the status code
	}
	return tx.Commit()
}

func createOrder(c *web.Ctx) error {
	tx := c.MustService(txKey)                     // this request's own transaction
	var id int64
	if err := tx.QueryRowContext(c.Context(),
		`INSERT INTO orders (sku, qty) VALUES ($1, $2) RETURNING id`,
		sku, qty).Scan(&id); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}
```

`kernel.Local()` gives you three things: the binding is visible only inside this request's **subtree** (parents and siblings cannot read it), it is removed when the scope is disposed, and it shadows a global binding of the same name. So "the transaction follows the request" is a scope-level fact, not a naming convention.

Three points worth keeping:

1. **Rollback is the middleware's job.** The `defer` covers both panics and early returns; a `Rollback` after a successful commit is `ErrTxDone`, so ignore it.
2. **Keep returning the error**, don't write the response from the middleware — the status code is decided by the central error mapper.
3. `BeginTx` takes **`c.Context()`**: that gives you cancellation, and it is the only safety net when you forget to roll back.

### Passing it explicitly

A transaction can also just be a value you pass down:

```go
func createOrder(c *web.Ctx) error {
	pool := c.MustService(dbKey)
	tx, err := pool.BeginTx(c.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := writeOrder(c.Context(), tx, in); err != nil {
		return err
	}
	if err := writeAudit(c.Context(), tx, in); err != nil {
		return err
	}
	return tx.Commit()
}

func writeOrder(ctx context.Context, tx *sql.Tx, in orderIn) error { ... }
```

The upside: "this path uses a transaction" is visible in the signature and you control when it commits. The cost: every call site has to pass it along, and one missed layer is a write that silently is not in the transaction.

### Which one

| Situation | Pick |
|---|---|
| Several write endpoints, all with the same boundary | Middleware plus `kernel.Local()` |
| One or two write endpoints, and you want "uses a transaction" in the signature | Explicit passing |
| Work after the commit (calling an external system, publishing a message) | Explicit passing (you own the ordering) |
| The transaction needs something another middleware put in the scope | Middleware plus `kernel.Local()` (same scope, visible) |

Both can coexist: the middleware covers the default path, and an individual endpoint can open its own transaction to step around it.

## Minimal examples for three stacks

All three have **exactly the same shape**; only the types and the call differ:

| | Pool | Transaction | Struct scanning |
|---|---|---|---|
| `database/sql` + pgx | `*sql.DB` | `*sql.Tx` | hand-written `Scan` |
| `sqlx` | `*sqlx.DB` | `*sqlx.Tx` | `GetContext` / `SelectContext` |
| GORM | `*gorm.DB` | `*gorm.DB` (a session) | model structs |

### database/sql + pgx

```go
import (
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	pool, err := sql.Open("pgx", dsn)          // the driver name comes from that blank import
	if err != nil {
		panic(err)
	}
	pool.SetMaxOpenConns(16)
	pool.SetMaxIdleConns(16)

	app := web.New()
	if _, err := kernel.Provide(app.Root(), dbKey, pool); err != nil {
		panic(err)
	}
	app.OnShutdown(func(context.Context) error { return pool.Close() })

	app.Use(TxMiddleware)                      // the one above
	app.POST("/orders", createOrder)
	app.Run(":8080")
}
```

### sqlx

```go
import (
	"github.com/jmoiron/sqlx"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type order struct {
	ID        int64     `db:"id" json:"id"`
	SKU       string    `db:"sku" json:"sku"`
	Qty       int       `db:"qty" json:"qty"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

var (
	dbKey = kernel.NewServiceKey[*sqlx.DB]("app.db")
	txKey = kernel.NewServiceKey[*sqlx.Tx]("app.tx")
)

func TxMiddleware(c *web.Ctx, next web.Handler) error {
	pool := c.MustService(dbKey)
	tx, err := pool.BeginTxx(c.Context(), nil) // sqlx's Begin has the extra x
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := kernel.Provide(c.Kernel(), txKey, tx, kernel.Local()); err != nil {
		return err
	}
	if err := next(c); err != nil {
		return err
	}
	return tx.Commit()
}

func createOrder(c *web.Ctx) error {
	tx := c.MustService(txKey)
	var o order
	// sqlx's payoff: one statement scans straight into a struct, no hand-written Scan list
	if err := tx.GetContext(c.Context(), &o,
		`INSERT INTO orders (sku, qty) VALUES ($1, $2) RETURNING id, sku, qty, created_at`,
		sku, qty); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, o)
}

func listOrders(c *web.Ctx) error {
	var rows []order
	if err := c.MustService(dbKey).SelectContext(c.Context(), &rows,
		`SELECT id, sku, qty, created_at FROM orders ORDER BY id`); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"orders": rows})
}
```

### GORM

```go
import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type Order struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	SKU       string    `json:"sku"`
	Qty       int       `json:"qty"`
	CreatedAt time.Time `json:"created_at"`
}

func (Order) TableName() string { return "orders" }   // do not let gorm guess a pluralised name

var (
	dbKey = kernel.NewServiceKey[*gorm.DB]("app.db")
	txKey = kernel.NewServiceKey[*gorm.DB]("app.tx")  // a transaction is also a *gorm.DB
)

func TxMiddleware(c *web.Ctx, next web.Handler) error {
	tx := c.MustService(dbKey).WithContext(c.Context()).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := kernel.Provide(c.Kernel(), txKey, tx, kernel.Local()); err != nil {
		return err
	}
	if err := next(c); err != nil {
		return err
	}
	return tx.Commit().Error
}

func main() {
	pool, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	// pool sizing happens on the underlying *sql.DB — gorm does not expose these
	sqlDB, err := pool.DB()
	if err != nil {
		panic(err)
	}
	sqlDB.SetMaxOpenConns(16)

	app := web.New()
	if _, err := kernel.Provide(app.Root(), dbKey, pool); err != nil {
		panic(err)
	}
	app.OnShutdown(func(context.Context) error { return sqlDB.Close() })  // still closing the underlying pool
	app.Use(TxMiddleware)
	app.Run(":8080")
}
```

`createOrder` is just `tx.WithContext(c.Context()).Create(&o)` — on Postgres gorm uses `INSERT … RETURNING` to fill the primary key back in, and the transaction semantics are identical to the other two stacks.

## Failure modes

### Forgetting to roll back: does the connection stay occupied

Set the pool's limit to 1, have the middleware neither commit nor roll back, and change exactly one thing — the ctx handed to `BeginTx`:

| ctx passed to `BeginTx` | `Stats().InUse` after the request | The next request |
|---|---|---|
| `c.Context()` | **0** (the connection came back on its own) | fine |
| `context.Background()` | **1** (held by that transaction) | `context deadline exceeded` |

With the request ctx, the standard library watches it: as soon as the request ends the ctx is cancelled, the unresolved transaction is rolled back and the connection is returned. **That is not a reason to skip the `defer`** (only you know whether it should commit), but it is a hard reason to always pass `c.Context()`.

### A commit-time failure: the response is already gone

That middleware commits after `next(c)` returns, while `c.JSON(201, …)` inside the handler has **already been written**. Postgres' deferred constraints (`UNIQUE … DEFERRABLE INITIALLY DEFERRED`) reproduce this window reliably: insert two rows with the same `sku` in one transaction, both INSERTs succeed, the 201 is settled, and the uniqueness conflict only surfaces at `Commit`.

The result: **the client got a 201 and the database has no rows** (the whole transaction rolls back, and the access log records the failed commit). To tighten it, have the handler return data and write the response after the commit. To accept it, accept it — a failed commit usually means the disk or the connection died, and a 500 would not have reached the client either.

### What happens when the pool is full

Borrowing a connection honours the ctx: at the pool's limit requests **queue** until the ctx handed to `BeginTx` is cancelled (client disconnect, an upstream timeout, `WithServer`'s `WriteTimeout`), and the failure looks like `context deadline exceeded` through the central error mapper. Size `SetMaxOpenConns` from the database's connection limit divided by your instance count, not from a number that felt big.

## Dependency versions

Every example on this page was run end to end against **PostgreSQL 18.3** (commit, rollback, panic safety net, commit-time failure, concurrency), with:

| Purpose | Module | Version |
|---|---|---|
| Postgres driver (shared by database/sql and sqlx) | `github.com/jackc/pgx/v5` | v5.11.0 |
| Struct scanning | `github.com/jmoiron/sqlx` | v1.4.0 |
| ORM | `gorm.io/gorm` | v1.31.2 |
| GORM's Postgres adapter | `gorm.io/driver/postgres` | v1.6.3 |

Switching to MySQL or SQLite touches only the import and the DSN: the pool, the transaction and the shutdown chain are unchanged, line for line.
