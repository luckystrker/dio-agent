// Подключение к PostgreSQL (pgx/v5) — единственная внешняя зависимость
// приложения (философия №6). Пул используется всеми модулями; singleton-lock
// и лимиты соединений приходят с этапами 9.7/9.x.
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultMaxConns — потолок соединений пула по умолчанию (DB_MAX_CONNS, 18.5).
	// Дефолт pgx — max(4, NumCPU): на большой машине пул растёт с числом ядер и
	// незаметно упирается в лимит соединений PostgreSQL при насыщении (открытые
	// курсоры + вторые запросы + фон + доставка). 8 — с запасом для одного
	// процесса dio-agent.
	DefaultMaxConns = 8

	// DefaultAcquireTimeout — граница ожидания свободного соединения (18.5).
	// В pgx v5 НЕТ настройки AcquireTimeout пула (это концепция v4): acquire
	// ждёт строго по контексту вызова (puddle v2), а контексты главных циклов
	// — context.WithCancel без дедлайна. Обёртка Pool ставит границу сама:
	// таймаут-контекст кладётся на заимствование соединения (выполнение
	// запроса идёт на исходном контексте). Насыщение пула превращается в
	// обычную ошибку запроса — существующие ретраи циклов её подхватывают.
	DefaultAcquireTimeout = 5 * time.Second
)

// Pool — пул соединений с ограниченным заимствованием: acquire (Query/QueryRow/
// Exec/Begin/Acquire) не ждёт свободного соединения дольше acquireTimeout,
// остальное — *pgxpool.Pool как есть (Close/Stat/Config/…).
type Pool struct {
	*pgxpool.Pool
	acquireTimeout time.Duration
}

// NewPool создаёт пул соединений по DSN из конфигурации. Необязательный
// maxConns — потолок соединений (значение из DB_MAX_CONNS, проверенное
// конфигом; 0 или не задан — DefaultMaxConns). Соединение сразу проверяется
// Ping-ом: невалидный DSN или недоступная база — явная ошибка запуска, а не
// молчаливый пул.
func NewPool(ctx context.Context, databaseURL string, maxConns ...int) (*Pool, error) {
	cfg, err := poolConfig(databaseURL, maxConns...)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL не создан: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL недоступен: %w", err)
	}
	return &Pool{Pool: pool, acquireTimeout: DefaultAcquireTimeout}, nil
}

// NewPoolFromConfig — пул из готовой конфигурации pgxpool (тесты собирают
// специальные конфигурации: одно соединение, редкие проверки здоровья).
func NewPoolFromConfig(ctx context.Context, cfg *pgxpool.Config) (*Pool, error) {
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("пул PostgreSQL не создан: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("PostgreSQL недоступен: %w", err)
	}
	return &Pool{Pool: pool, acquireTimeout: DefaultAcquireTimeout}, nil
}

// SetAcquireTimeout — граница ожидания соединения (тесты сжимают).
func (p *Pool) SetAcquireTimeout(d time.Duration) {
	if d > 0 {
		p.acquireTimeout = d
	}
}

// acquire заимствует соединение не дольше acquireTimeout (или ближе — если
// исходный контекст истекает раньше): насыщение пула — обычная ошибка с
// context.DeadlineExceeded внутри, а не молчаливое зависание.
func (p *Pool) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	acqCtx, cancel := context.WithTimeout(ctx, p.acquireTimeout)
	defer cancel()
	c, err := p.Pool.Acquire(acqCtx)
	if err != nil {
		return nil, fmt.Errorf("пул: соединение не заимствовано за %s (пул насыщен или база недоступна): %w",
			p.acquireTimeout, err)
	}
	return c, nil
}

// Аккуратные копии семантики pgxpool (внутренние poolRows/poolRow/Tx недоступны
// вне пакета): Query/QueryRow возвращают соединение по закрытию строк, Exec —
// по завершении, транзакция — по Commit/Rollback. Воспроизведены в dbrows.go.

// Query — запрос со строками: acquire ограничен, соединение возвращается по
// Close/исчерпанию строк.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return errRows{err: err}, err
	}
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		c.Release()
		return errRows{err: err}, err
	}
	return &poolRows{r: rows, c: c}, nil
}

// QueryRow — запрос одной строки: соединение возвращается по Scan.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c, err := p.acquire(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return &poolRow{r: c.QueryRow(ctx, sql, args...), c: c}
}

// Exec — запрос без строк: соединение возвращается по завершении.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer c.Release()
	return c.Exec(ctx, sql, args...)
}

// Begin открывает транзакцию (соединение — по Commit/Rollback).
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.BeginTx(ctx, pgx.TxOptions{})
}

// BeginTx открывает транзакцию с режимом; Commit/Rollback возвращают соединение.
func (p *Pool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.BeginTx(ctx, opts)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &poolTx{Tx: tx, c: c}, nil
}

// Acquire — явное заимствование с той же границей ожидания (миграции,
// держатели соединений в тестах).
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) { return p.acquire(ctx) }

// poolConfig разбирает DSN и назначает потолок соединений пула. Живой базы не
// требует — чистый разбор (юнит-тест 18.5: значение доезжает до конфигурации).
func poolConfig(databaseURL string, maxConns ...int) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL не разбирается: %w", err)
	}
	n := DefaultMaxConns
	if len(maxConns) > 0 && maxConns[0] > 0 {
		n = maxConns[0]
	}
	cfg.MaxConns = int32(n)
	return cfg, nil
}
