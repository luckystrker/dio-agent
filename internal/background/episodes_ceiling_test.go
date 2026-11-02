// Юнит-тесты потолка выборки pendingEpisodes (17.14) без БД: фейковый
// queryer фиксирует SQL и аргументы — полный скан social_events не должен
// возвращаться ни при каком лимите эпизодов.
package background

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// captureQueryer запоминает последний Query (SQL + аргументы).
type captureQueryer struct {
	sql  string
	args []any
}

func (c *captureQueryer) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql, c.args = sql, args
	return emptyRows{}, nil
}

func (c *captureQueryer) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }

// emptyRows — пустая выдача pgx.Rows.
type emptyRows struct{}

func (emptyRows) Close()                                       {}
func (emptyRows) Err() error                                   { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (emptyRows) Next() bool                                   { return false }
func (emptyRows) Scan(_ ...any) error                          { return nil }
func (emptyRows) Values() ([]any, error)                       { return nil, nil }
func (emptyRows) RawValues() [][]byte                          { return nil }
func (emptyRows) Conn() *pgx.Conn                              { return nil }

// TestPendingEpisodesSelectCeiling — выборка событий ограничена потолком
// limit*50: до фикта лимит применялся после выборки, и стоимость такта росла
// с историей (полный скан social_events каждые 15–30 с).
func TestPendingEpisodesSelectCeiling(t *testing.T) {
	q := &captureQueryer{}
	if _, err := pendingEpisodes(context.Background(), q, time.Now(), 10); err != nil {
		t.Fatalf("pendingEpisodes: %v", err)
	}
	if !strings.Contains(strings.ToUpper(q.sql), "LIMIT") {
		t.Fatalf("SELECT необработанных событий без потолка: %s", q.sql)
	}
	if len(q.args) != 1 {
		t.Fatalf("аргументов запроса %d (%v), хочу один — потолок выборки", len(q.args), q.args)
	}
	ceiling, ok := q.args[0].(int)
	if !ok {
		t.Fatalf("потолок не int: %T %v", q.args[0], q.args[0])
	}
	if ceiling != 10*episodeSelectFactor {
		t.Fatalf("потолок = %d, хочу %d (limit*%d)", ceiling, 10*episodeSelectFactor, episodeSelectFactor)
	}
}
