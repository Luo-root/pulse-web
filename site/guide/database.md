# 数据库集成

框架里**没有**任何 DB 相关的东西：没有仓储接口、没有 ORM 包装、没有 `WithDatabase`。这一页讲的不是 API，而是把 `database/sql` 或第三方 DB 库接进来时**东西该摆在哪、失败时会怎样**——两句话的判据打头，三种栈的完整示例垫后，中间是事务。

## 两句话的判据

**连接池是进程级资源，事务是请求级资源。** 前者进装配面（`app.Root()`），后者进请求作用域（`c.Kernel()`）。摆错了不会立刻报错，只会在「并发一上来就慢」或者「池越开越多」的时候才显形。

## 连接池进装配面

```go
var dbKey = kernel.NewServiceKey[*sql.DB]("app.db")

func main() {
	pool, err := sql.Open("pgx", dsn)     // 或 sqlx.Connect / gorm.Open
	if err != nil {
		panic(err)
	}
	pool.SetMaxOpenConns(16)              // 池参数与请求无关，装配期定死
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
	pool := c.MustService(dbKey)          // 全局绑定：任何作用域都读得到
	rows, err := pool.QueryContext(c.Context(), `SELECT id, sku FROM orders`)
	...
}
```

**别把池 Provide 到请求作用域**：`c.Kernel()` 是请求 scope，随请求销毁——在那里 Provide 池就是「每个请求各建一个池」。

**池由谁关**：`kernel.Provide` 只登记绑定，**不会**替你关 `io.Closer`（dispose 只是把服务从仓库里撤掉）。池的生命周期归装配方，所以关它必须显式写。

### 宿主已经有池

`web.WithRoot(k)` 接入宿主那棵 kernel 树，再往同一棵树上 `Provide` 同一个键——两个组件读同一个池，谁也不拥有谁：

```go
k := kernel.New()                      // 宿主的根（比如已有装配的进程）
kernel.Provide(k, dbKey, hostPool)     // 宿主把自己那个池登记进去

app := web.New(web.WithRoot(k))        // 框架接进同一棵树
// 之后 app.Root() == k，handler 里 c.MustService(dbKey) 拿到的就是 hostPool
```

注意 `Run` / `Serve` 返回时会**级联销毁**这棵树，关池仍然由关它的人负责（这里是宿主）。

### 关池排在关闭时序的第 ③ 步

```
srv.Shutdown（drain 在飞请求）→ OnShutdown 回调 → root.Dispose → 出口 flush
```

池放进 `OnShutdown`：那时在飞请求已经结束（没人再借连接），而 `root.Dispose` 还没来。完整时序与预算见[装配与运行](/guide/assembly)。

## 请求级事务

### 中间件 + `kernel.Local()`

一手开事务、一手把它绑进**本请求**的作用域，handler 只管用：

```go
var txKey = kernel.NewServiceKey[*sql.Tx]("app.tx")

func TxMiddleware(c *web.Ctx, next web.Handler) error {
	pool := c.MustService(dbKey)
	tx, err := pool.BeginTx(c.Context(), nil)      // ctx 用请求的，理由见「失败模式」
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()           // panic 与提前返回都兜住；提交后有 ErrTxDone，忽略
	if _, err := kernel.Provide(c.Kernel(), txKey, tx, kernel.Local()); err != nil {
		return err
	}
	if err := next(c); err != nil {
		return err                                 // 交回统一错误映射决定状态码
	}
	return tx.Commit()
}

func createOrder(c *web.Ctx) error {
	tx := c.MustService(txKey)                     // 本请求自己的那个事务
	var id int64
	if err := tx.QueryRowContext(c.Context(),
		`INSERT INTO orders (sku, qty) VALUES ($1, $2) RETURNING id`,
		sku, qty).Scan(&id); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, map[string]any{"id": id})
}
```

`kernel.Local()` 保证三件事：绑定只在本请求**子树**可见（父与兄弟读不到）、随 scope 销毁自动撤除、同名时遮蔽全局绑定。所以「事务跟着请求走」是作用域事实，不是命名约定。

四条要点：

1. **回滚责任在中间件**。`defer` 同时兜住 panic 与提前返回；提交之后再 `Rollback` 是 `ErrTxDone`，忽略即可。
2. **错误继续往上抛**，别在中间件里写响应——状态码由统一错误映射决定。
3. `BeginTx` **传 `c.Context()`**：它既是取消语义，也是你忘记回滚时唯一的兜底。
4. **handler 取事务要走同一个键**（`c.MustService(txKey)`）。绕过它直接查池，等于同一个请求里同时要两条连接——池上限紧的时候会自己把自己等死（实测：池上限 1 时那个请求永久等待，栈停在 `DB.conn` 上等连接，而那条连接正被本请求自己的事务握着）。

::: warning 别在流式响应里持有事务
SSE / 流式响应的 handler 会跑几秒到几分钟，而上面这个中间件**要等 handler 返回才提交**——这期间事务一直握着自己那条连接。实测：池上限 1 时，一条 1.85 秒的流让期间的普通请求排队 **1.45 秒**（等流结束、事务提交，它才拿到连接）。按池上限以上的并发量堆这类流，后面的请求只能排队到 `context deadline exceeded`。

要流式输出，就把事务收在「取数」那一段：**先查完、提交，再开始写流**，别让事务横跨整条响应。
:::

### 显式传递

事务也可以只是个普通值，一路传下去：

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

好处是「用了事务」在签名上看得见、提交时机由你定；代价是每个调用点都要想着把它传下去，漏一层就是一个不参与事务的写。

### 选哪个

| 情况 | 选 |
|---|---|
| 写端点不止一两个、事务边界都相同 | 中间件 + `kernel.Local()` |
| 只有一两个写端点，想让「用了事务」写在签名上 | 显式传递 |
| 提交之后还要干别的（写外部系统、发消息） | 显式传递（顺序由你控制） |
| 事务里要读别的中间件放进作用域的东西 | 中间件 + `kernel.Local()`（同 scope 可见） |

两者可以并存：中间件给默认路径，个别端点自己开事务绕开它。

### 同一个键：读写分离与「强制走主库」

上面的例子用两个键（池一个、事务一个）。也可以**只用一个键**——把它声明成接口，装配面绑只读池（副本），事务中间件往同一个键上 `Local()` 绑事务：

```go
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var dbKey = kernel.NewServiceKey[Querier]("app.db")

// 装配面：只读池（副本）——没挂事务的请求永远打到这里
kernel.Provide[Querier](app.Root(), dbKey, readPool)

// 中间件：事务绑到**同一个键**上，本请求子树读到它（同名遮蔽全局）
kernel.Provide[Querier](c.Kernel(), dbKey, tx, kernel.Local())
```

于是「这个请求走主库还是副本」由**有没有挂事务中间件**决定，handler 里始终只有一行 `c.MustService(dbKey)`。实测语义（同一份 PG）：没挂事务的请求读到 `*sql.DB`、挂了的读到 `*sql.Tx`；事务里写的行**本请求内可见**、**提交前别的连接看不到**、提交后可见。

一个语法注意：绑接口键要显式写类型参数 `kernel.Provide[Querier]`——泛型默认从值上推 `T`，`*sql.Tx` 与 `ServiceKey[Querier]` 对不上。

## 三种栈的最小示例

三种栈的**形状完全一样**，差别只在类型与语句：

| | 池 | 事务 | 结构体扫描 |
|---|---|---|---|
| `database/sql` + pgx | `*sql.DB` | `*sql.Tx` | 手写 `Scan` |
| `sqlx` | `*sqlx.DB` | `*sqlx.Tx` | `GetContext` / `SelectContext` |
| GORM | `*gorm.DB` | `*gorm.DB`（Session） | 模型结构体 |

### database/sql + pgx

```go
import (
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	pool, err := sql.Open("pgx", dsn)          // 驱动名来自上面的匿名 import
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

	app.Use(TxMiddleware)                      // 上面那段
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
	tx, err := pool.BeginTxx(c.Context(), nil) // sqlx 的 Begin：多一个 x
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
	// sqlx 的价值：一条语句直接落进结构体，不用手写 Scan 列表
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

::: warning GORM 的池参数只在底层 `*sql.DB` 上设
`*gorm.DB` 上**没有** `SetMaxOpenConns` / `SetMaxIdleConns` / `SetConnMaxLifetime` 这类方法（实测 v1.31.2：唯一的 `Set*` 是 `SetupJoinTable`），`gorm.Config` 也只提供 `ConnPool` 这一个注入口。所以不拿 `db.DB()` 回来设参时，你用的就是标准库默认池——**`MaxOpenConns` 不限**（实测 `Stats().MaxOpenConnections == 0`）、`MaxIdleConns` 为 2。压测一来就是连接打满数据库，而这**没有任何编译期或运行期提示**：代码看着像配过了，其实没配。
:::

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

func (Order) TableName() string { return "orders" }   // 不让 gorm 复数化去猜表名

var (
	dbKey = kernel.NewServiceKey[*gorm.DB]("app.db")
	txKey = kernel.NewServiceKey[*gorm.DB]("app.tx")  // 事务也是 *gorm.DB
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
	// 池参数要回到底层 *sql.DB 上调——gorm 不暴露这几个
	sqlDB, err := pool.DB()
	if err != nil {
		panic(err)
	}
	sqlDB.SetMaxOpenConns(16)

	app := web.New()
	if _, err := kernel.Provide(app.Root(), dbKey, pool); err != nil {
		panic(err)
	}
	app.OnShutdown(func(context.Context) error { return sqlDB.Close() })  // 关的仍是底层池
	app.Use(TxMiddleware)
	app.Run(":8080")
}
```

`createOrder` 里就是 `tx.WithContext(c.Context()).Create(&o)`——Postgres 下 gorm 用 `INSERT … RETURNING` 回填主键，事务语义与上面两栈完全一致。

## 失败模式

### 忘了回滚：连接会不会被占住

把池压到上限 1，中间件故意既不提交也不回滚，只改一件事——`BeginTx` 收到的 ctx：

| `BeginTx` 的 ctx | 请求结束后 `Stats().InUse` | 下一个请求 |
|---|---|---|
| `c.Context()` | **0**（连接自己回来了） | 正常 |
| `context.Background()` | **1**（被那个事务占住） | `context deadline exceeded` |

传了请求 ctx 时，标准库盯着它：请求一结束 ctx 被取消，未了结的事务自动回滚并归还连接。**这不是让你省掉 `defer` 的理由**（提交与否仍然只有你知道），但它是「Always 传 `c.Context()`」的硬理由。

### 提交期失败：响应已经发出去了

上面那段中间件在 `next(c)` 返回之后才 `Commit`，而 handler 里 `c.JSON(201, …)` **已经写出去了**。用 Postgres 的延迟约束（`UNIQUE … DEFERRABLE INITIALLY DEFERRED`）能把这个窗口稳定复现：同一事务插两行同 `sku`，两条 INSERT 都成功、响应 201 落定，`Commit` 时才报唯一冲突。

结果：**客户端拿到 201，库里一行没有**（事务整体回滚，访问日志里记着这次提交失败）。要收紧就让 handler 只返回数据、把响应写出放在提交之后；要接受就接受——提交失败通常意味着磁盘或连接出了事，那时 500 也已经到不了客户端。

### 池满了会怎样

借连接这一步认 ctx：池到上限时请求**排队**，排到 `BeginTx` 的 ctx 被取消为止（客户端断开、上游超时、`WithServer` 的 `WriteTimeout` 都可能触发），失败形态是 `context deadline exceeded`，走统一错误映射。所以 `SetMaxOpenConns` 要按数据库的连接上限除以实例数来定，而不是拍一个大的。

## 依赖版本

本页示例在 **PostgreSQL 18.3** 上逐条跑通过（提交 / 回滚 / panic 兜底 / 提交期失败 / 并发），依赖版本：

| 用途 | 模块 | 版本 |
|---|---|---|
| Postgres 驱动（database/sql 与 sqlx 共用） | `github.com/jackc/pgx/v5` | v5.11.0 |
| 结构体扫描 | `github.com/jmoiron/sqlx` | v1.4.0 |
| ORM | `gorm.io/gorm` | v1.31.2 |
| GORM 的 Postgres 适配 | `gorm.io/driver/postgres` | v1.6.3 |

换成 MySQL / SQLite 只动 import 与 DSN：池、事务、关闭时序这三段代码一个字都不用改。
