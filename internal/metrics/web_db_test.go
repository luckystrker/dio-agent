//go:build db

// БД-тест метрик веба и картинок (этап 36.2): поиски разговорного пула и
// отправленные картинки считаются из базы (философия №2), а не из памяти
// процесса; лимит поисков виден в снимке.
package metrics

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWebMetricsFromDatabase(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO web_usage (kind, period, count)
		VALUES ('search', (now() AT TIME ZONE 'UTC')::date, 4)`); err != nil {
		t.Fatalf("счётчик поисков не вставлен: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, updated_at)
		VALUES (900, 1, 42, '{"kind":"photo"}'::jsonb, 'sent', now()),
		       (900, 0, 42, '{"kind":"message","text":"раз"}'::jsonb, 'sent', now())`); err != nil {
		t.Fatalf("доставки не вставлены: %v", err)
	}

	snap, err := NewPgSource(pool, SourceOptions{}).Snapshot(ctx)
	if err != nil {
		t.Fatalf("снимок не собран: %v", err)
	}
	if snap.Web.SearchesToday != 4 {
		t.Fatalf("поисков сегодня %d, хочу 4", snap.Web.SearchesToday)
	}
	if snap.Web.SearchesLimit != 15 {
		t.Fatalf("лимит поисков %d, хочу 15", snap.Web.SearchesLimit)
	}
	if snap.Web.ImagesSent24h != 1 {
		t.Fatalf("картинок отправлено за сутки %d, хочу 1", snap.Web.ImagesSent24h)
	}

	// Вчерашние поиски и картинки в сегодняшние цифры не попадают.
	if _, err := pool.Exec(ctx, `
		UPDATE web_usage SET period = (now() AT TIME ZONE 'UTC')::date - 1 WHERE kind = 'search';
		UPDATE delivery_outbox SET updated_at = now() - interval '2 days' WHERE payload->>'kind' = 'photo'`); err != nil {
		t.Fatalf("сдвиг времени не выполнен: %v", err)
	}
	snap, err = NewPgSource(pool, SourceOptions{}).Snapshot(ctx)
	if err != nil {
		t.Fatalf("снимок не собран: %v", err)
	}
	if snap.Web.SearchesToday != 0 || snap.Web.ImagesSent24h != 0 {
		t.Fatalf("вчерашние события попали в сутки: %+v", snap.Web)
	}

	var buf bytes.Buffer
	if err := WritePrometheus(&buf, snap); err != nil {
		t.Fatalf("экспозиция не записана: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"dio_web_searches_today", "dio_web_searches_limit 15", "dio_images_sent_24h"} {
		if !strings.Contains(out, want) {
			t.Fatalf("в экспозиции нет %q:\n%s", want, out)
		}
	}
}
