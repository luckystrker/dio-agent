// Общий интерфейс доступа к БД для репозиториев памяти: и пул pgx, и
// транзакция pgx удовлетворяют ему (sqlc-стиль). Записи идут в транзакции
// (одна запись хода — одна транзакция, §10.1), чтения — прямо с пула.
package memory

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX — минимальный набор запросов pgx, нужный репозиториям.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// Псевдонимы одиночных строк/наборов для общих сканеров.
type (
	pgxRow  = pgx.Row
	pgxRows = pgx.Rows
)

// isNoRows отличает «строки нет» от прочих ошибок запроса.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
