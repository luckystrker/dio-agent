//go:build db

// БД-тест суточного учёта поисков разговорного пула (этап 36.1): счётчик
// живёт в базе (философия №2) и переживает рестарт; сутки — UTC, как у
// модельных бюджетов §16.
package web

import (
	"context"
	"testing"
)

func TestUsageStoreSearchCounterPerDay(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := UsageStore{}

	used, err := store.SearchesToday(ctx, pool)
	if err != nil {
		t.Fatalf("счётчик не прочитан: %v", err)
	}
	if used != 0 {
		t.Fatalf("до поисков used = %d, хочу 0", used)
	}

	for i := 0; i < 3; i++ {
		if err := store.IncrementSearch(ctx, pool); err != nil {
			t.Fatalf("поиск %d не учтён: %v", i+1, err)
		}
	}
	used, err = store.SearchesToday(ctx, pool)
	if err != nil {
		t.Fatalf("счётчик не прочитан: %v", err)
	}
	if used != 3 {
		t.Fatalf("после трёх поисков used = %d, хочу 3", used)
	}

	// Счётчик привязан к суткам: вчерашние поиски не расходуют сегодняшний лимит.
	if _, err := pool.Exec(ctx, `
		UPDATE web_usage SET period = (now() AT TIME ZONE 'UTC')::date - 1
		WHERE kind = $1`, UsageSearch); err != nil {
		t.Fatalf("сдвиг суток не выполнен: %v", err)
	}
	used, err = store.SearchesToday(ctx, pool)
	if err != nil {
		t.Fatalf("счётчик не прочитан: %v", err)
	}
	if used != 0 {
		t.Fatalf("после сдвига суток used = %d, хочу 0", used)
	}
}
