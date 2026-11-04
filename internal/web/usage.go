// Учёт веб-поисков разговорного пула (этап 36.1): суточный лимит §16 — часть
// бюджета, поэтому счётчик живёт в базе (философия №2) и переживает рестарт.
// Сутки — UTC, как у модельных бюджетов.
package web

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// UsageSearch — вид учёта «поиск» в web_usage.
const UsageSearch = "search"

// UsageDB — минимальный интерфейс БД для счётчика: пул и транзакция подходят.
type UsageDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UsageStore — счётчик поисков разговорного пула по суткам.
type UsageStore struct{}

// IncrementSearch учитывает один выполненный поиск.
func (UsageStore) IncrementSearch(ctx context.Context, db UsageDB) error {
	_, err := db.Exec(ctx, `
		INSERT INTO web_usage (kind, period, count, updated_at)
		VALUES ($1, (now() AT TIME ZONE 'UTC')::date, 1, now())
		ON CONFLICT (kind, period)
		DO UPDATE SET count = web_usage.count + 1, updated_at = now()`, UsageSearch)
	if err != nil {
		return fmt.Errorf("web: поиск не учтён: %w", err)
	}
	return nil
}

// SearchesToday — сколько поисков разговорного пула уже сделано сегодня (UTC).
func (UsageStore) SearchesToday(ctx context.Context, db UsageDB) (int64, error) {
	var used int64
	err := db.QueryRow(ctx, `
		SELECT count FROM web_usage
		WHERE kind = $1 AND period = (now() AT TIME ZONE 'UTC')::date`, UsageSearch).Scan(&used)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("web: счётчик поисков не прочитан: %w", err)
	}
	return used, nil
}
