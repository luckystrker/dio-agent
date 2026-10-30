//go:build db

// БД-тесты ingest §6.1: дедупликация update (UNIQUE telegram_update_id =
// «уже сохранено», не ошибка), offset двигается только вместе с записью
// (одна транзакция) и только вперёд, unsupported/journal_only не застревают.
package telegram

import (
	"context"
	"testing"
)

func TestIngestInsertsEventAndMovesOffset(t *testing.T) {
	pool := newTestPool(t)
	res := mustIngest(t, pool, rawMessageOf(100, 42, "привет"))

	if !res.Inserted || res.EventID == 0 {
		t.Fatalf("событие не вставлено: %+v", res)
	}
	if res.Status != StatusPending {
		t.Fatalf("личка должна быть pending: %+v", res.Status)
	}

	// §6.1 шаг 5: следующий offset записан в runtime_state.
	offset, has, err := ReadOffset(context.Background(), pool)
	if err != nil || !has {
		t.Fatalf("offset не читается: has=%v err=%v", has, err)
	}
	if offset != 101 {
		t.Fatalf("offset = %d, хочу 101", offset)
	}

	// Сырой update сохранён (jsonb) — восстановление читает факт, не пересказ.
	var raw string
	if err := pool.QueryRow(context.Background(),
		`SELECT raw_update::text FROM social_events WHERE id = $1`, res.EventID).Scan(&raw); err != nil {
		t.Fatalf("raw_update не читается: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("raw_update пуст")
	}
}

func TestIngestDuplicateIsNotAnError(t *testing.T) {
	// §21.1 «Сбой после сохранения update»: update не теряется, дедупликация
	// не создаёт второе событие.
	pool := newTestPool(t)
	ctx := context.Background()
	raw := rawMessageOf(200, 42, "раз")

	mustIngest(t, pool, raw)
	second := mustIngest(t, pool, raw)

	// Дубль: «уже сохранено» — не ошибка, без новой строки и без id
	// (паритет TS: eventId null у дубля).
	if second.Inserted {
		t.Fatalf("дубль вставлен второй раз: %+v", second)
	}
	if second.EventID != 0 {
		t.Fatalf("у дубля не должно быть id: %+v", second)
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE telegram_update_id = 200`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("строк с update 200: %d (err=%v)", count, err)
	}
	// Offset не откатился и не задвоился.
	offset, _, err := ReadOffset(ctx, pool)
	if err != nil || offset != 201 {
		t.Fatalf("offset после дубля = %d (err=%v)", offset, err)
	}
}

func TestIngestOffsetOnlyGrows(t *testing.T) {
	// Пачки могут пересекаться: старый update не откатывает offset (GREATEST).
	pool := newTestPool(t)
	mustIngest(t, pool, rawMessageOf(300, 42, "первый"))
	mustIngest(t, pool, rawMessageOf(299, 42, "поздний дубль пачки"))

	offset, _, err := ReadOffset(context.Background(), pool)
	if err != nil || offset != 301 {
		t.Fatalf("offset = %d (err=%v), хочу 301", offset, err)
	}
}

func TestIngestUnsupportedAndGroupNoiseDoNotStallOffset(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// callback_query — unsupported: в журнал journal_only, offset двигается (§6.1).
	unsupported := mustIngest(t, pool, []byte(`{"update_id":400,"callback_query":{"id":"q"}}`))
	if unsupported.Status != StatusJournalOnly || unsupported.Event.Supported {
		t.Fatalf("unsupported должен быть journal_only: %+v", unsupported)
	}
	var chatID int64
	if err := pool.QueryRow(ctx,
		`SELECT chat_id FROM social_events WHERE telegram_update_id = 400`).Scan(&chatID); err != nil || chatID != 0 {
		t.Fatalf("unsupported под фиктивным чатом 0: chat=%d err=%v", chatID, err)
	}

	// Групповой шум — journal_only (§6.3), offset тоже двигается.
	noise := mustIngest(t, pool, []byte(`{"update_id":401,"message":{"message_id":1,"from":{"id":7,"first_name":"П"},"chat":{"id":-100,"type":"group"},"text":"болтовня"}}`))
	if noise.Status != StatusJournalOnly {
		t.Fatalf("групповой шум должен быть journal_only: %+v", noise)
	}

	offset, _, err := ReadOffset(ctx, pool)
	if err != nil || offset != 402 {
		t.Fatalf("offset = %d (err=%v), хочу 402 — журнал не должен застревать", offset, err)
	}
}

func TestIngestUnparsableUpdateRejected(t *testing.T) {
	// Неразборчивый update (нет update_id) не записывается и не двигает offset.
	pool := newTestPool(t)
	ctx := context.Background()
	mustIngest(t, pool, rawMessageOf(500, 42, "до"))

	if _, err := IngestUpdate(ctx, pool, []byte(`{"foo":1}`)); err == nil {
		t.Fatalf("update без update_id должен давать ошибку")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("неразборчивый update не должен вставляться: строк %d (err=%v)", count, err)
	}
	offset, _, err := ReadOffset(ctx, pool)
	if err != nil || offset != 501 {
		t.Fatalf("offset не должен измениться ошибкой нормализации: %d (err=%v)", offset, err)
	}
}

func TestIngestDBFailureDoesNotMoveOffset(t *testing.T) {
	// §6.1: при ошибке БД offset не продвигается. Имитация — закрытый пул:
	// транзакция ingest падает целиком (событие + offset атомарны).
	pool := newTestPool(t)
	mustIngest(t, pool, rawMessageOf(600, 42, "до"))

	broken := newTestPool(t) // отдельная база, пул закроем
	broken.Close()

	if _, err := IngestUpdate(context.Background(), broken, rawMessageOf(601, 42, "после сбоя")); err == nil {
		t.Fatalf("ingest в закрытый пул должен падать")
	}

	// В живой базе ничего не изменилось.
	offset, _, err := ReadOffset(context.Background(), pool)
	if err != nil || offset != 601 {
		t.Fatalf("offset живой базы = %d (err=%v)", offset, err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("событие сбойной попытки не должно попасть в живую базу: %d", count)
	}
}
