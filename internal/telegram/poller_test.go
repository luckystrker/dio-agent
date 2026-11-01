// Юнит-тесты политики polling (16.4) без БД и сети: нарастание паузы 409
// до потолка и классификация ошибок данных update'а (скип против повтора).
package telegram

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestConflictBackoffGrowsToCap(t *testing.T) {
	opts := PollerOptions{ErrorBackoff: 3 * time.Second, ConflictBackoffCap: 30 * time.Second}
	want := []time.Duration{
		3 * time.Second, // первый конфликт — обычная пауза ошибки
		6 * time.Second, // дальше — вдвое с каждым подряд идущим
		12 * time.Second,
		24 * time.Second,
		30 * time.Second, // потолок
		30 * time.Second, // и не выше
	}
	for i, w := range want {
		if got := conflictBackoff(opts, i); got != w {
			t.Fatalf("конфликт №%d: пауза %v, хочу %v", i+1, got, w)
		}
	}
}

func TestUnusableUpdateData(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"нормализация: нет update_id", fmt.Errorf("%w: у update нет update_id", ErrUnusableUpdate), true},
		{"NUL в text (22021)", &pgconn.PgError{Code: "22021", Message: "invalid byte sequence"}, true},
		{"jsonb-escape (22P05)", &pgconn.PgError{Code: "22P05", Message: "unsupported Unicode escape sequence"}, true},
		{"нарушение ограничения (23502)", &pgconn.PgError{Code: "23502"}, true},
		{"ошибка данных, завёрнутая в ingest", fmt.Errorf("ingest: событие не записано: %w", &pgconn.PgError{Code: "22021"}), true},
		{"транзиент БД (57P01)", &pgconn.PgError{Code: "57P01"}, false},
		{"обрыв соединения", errors.New("closed pool"), false},
	}
	for _, c := range cases {
		if got := unusableUpdateData(c.err); got != c.want {
			t.Fatalf("%s: unusable=%v, хочу %v", c.name, got, c.want)
		}
	}
}
