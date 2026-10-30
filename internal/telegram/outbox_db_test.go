//go:build db

// БД-тесты outbox §13.1–13.2: захват по порядку чата, отсутствие дублей
// захвата, условная запись sent, unknown-политика с отменой зависимых,
// восстановление sending после сбоя процесса (§12.2).
package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertOutbox вставляет строку outbox напрямую (создание строк — зона 9.4;
// здесь важна машина статусов).
func insertOutbox(t *testing.T, pool *pgxpool.Pool, turnID, chatID int64, position int, payload string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO delivery_outbox (turn_id, position, chat_id, payload) VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
		turnID, position, chatID, payload).Scan(&id); err != nil {
		t.Fatalf("outbox-строка не вставлена: %v", err)
	}
	return id
}

// mustClaim захватывает очередь, роняя тест при ошибке.
func mustClaim(t *testing.T, repo *OutboxRepo, limit int) []OutboxItem {
	t.Helper()
	items, err := repo.ClaimDue(context.Background(), limit)
	if err != nil {
		t.Fatalf("claim не удался: %v", err)
	}
	return items
}

func TestClaimDueOrderAndNoDoubleClaim(t *testing.T) {
	// §13.1: перед сетью sending; порядок внутри чата (chat, turn, position);
	// захваченное не выдаётся второму такту — дублей доставки нет.
	pool := newTestPool(t)
	repo := NewOutboxRepo(pool)

	idB1 := insertOutbox(t, pool, 11, -5, 0, `{"kind":"message","text":"группа"}`)
	idA0 := insertOutbox(t, pool, 10, 42, 0, `{"kind":"message","text":"первый"}`)
	idA1 := insertOutbox(t, pool, 10, 42, 1, `{"kind":"message","text":"второй"}`)

	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 3 {
		t.Fatalf("захвачено %d, хочу 3", len(claimed))
	}
	// Порядок доставки: по числовому возрастанию chat_id (группа -5 раньше
	// лички 42 — паритет сортировки TS), внутри чата — по позиции.
	if claimed[0].ID != idB1 || claimed[1].ID != idA0 || claimed[2].ID != idA1 {
		t.Fatalf("порядок доставки нарушен: %+v", claimed)
	}
	for _, item := range claimed {
		if item.Status != OutboxSending || item.Attempts != 1 {
			t.Fatalf("захваченное должно быть sending/1: %+v", item)
		}
	}

	// Повторный захват пуст: строки в sending не пере захватываются.
	if again := mustClaim(t, repo, 10); len(again) != 0 {
		t.Fatalf("второй захват выдал те же строки: %+v", again)
	}
}

func TestClaimDueRespectsRetryWaitNotBefore(t *testing.T) {
	// retry_wait с будущим not_before не захватывается; наступивший — да.
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	insertOutbox(t, pool, 20, 42, 0, `{"kind":"message","text":"позже"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'retry_wait', not_before = now() + interval '1 hour'`); err != nil {
		t.Fatalf("retry_wait не выставлен: %v", err)
	}
	if claimed := mustClaim(t, repo, 10); len(claimed) != 0 {
		t.Fatalf("not_before в будущем проигнорирован: %+v", claimed)
	}
	if _, err := pool.Exec(ctx, `UPDATE delivery_outbox SET not_before = now() - interval '1 second'`); err != nil {
		t.Fatalf("not_before не передвинут: %v", err)
	}
	if claimed := mustClaim(t, repo, 10); len(claimed) != 1 {
		t.Fatalf("наступивший retry_wait не захвачен: %+v", claimed)
	}
}

func TestMarkSentConditional(t *testing.T) {
	// §13.1: после подтверждённого успеха — Telegram ID и sent; условная
	// запись: отменённая строка из устаревшей памяти не перезаписывается.
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	id := insertOutbox(t, pool, 30, 42, 0, `{"kind":"message","text":"ответ"}`)
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 {
		t.Fatalf("захват: %+v", claimed)
	}
	messageID := int64(555)
	if err := repo.MarkSent(ctx, id, &messageID); err != nil {
		t.Fatalf("sent не записан: %v", err)
	}
	var status string
	var telegramID *int64
	if err := pool.QueryRow(ctx,
		`SELECT status, telegram_message_id FROM delivery_outbox WHERE id = $1`, id).Scan(&status, &telegramID); err != nil {
		t.Fatalf("строка не читается: %v", err)
	}
	if status != "sent" || telegramID == nil || *telegramID != 555 {
		t.Fatalf("sent: status=%s telegram_id=%v", status, telegramID)
	}

	// Отменённая строка: markSent обязан отказать.
	id2 := insertOutbox(t, pool, 31, 42, 0, `{"kind":"message","text":"отменят"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'cancelled' WHERE id = $1`, id2); err != nil {
		t.Fatalf("cancelled не выставлен: %v", err)
	}
	if err := repo.MarkSent(ctx, id2, &messageID); err == nil {
		t.Fatalf("sent на отменённой строке должен отказать")
	}
}

func TestMarkUnknownCancelsDependents(t *testing.T) {
	// §13.2: timeout после возможной отправки — unknown без автоповтора,
	// оставшиеся зависимые пузыри последовательности отменяются.
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	id0 := insertOutbox(t, pool, 40, 42, 0, `{"kind":"message","text":"пузырь 1"}`)
	id1 := insertOutbox(t, pool, 40, 42, 1, `{"kind":"message","text":"пузырь 2"}`)
	id2 := insertOutbox(t, pool, 40, 42, 2, `{"kind":"message","text":"пузырь 3"}`)
	// Чужой turn того же чата не зависит от чужой последовательности.
	other := insertOutbox(t, pool, 41, 42, 0, `{"kind":"message","text":"другой ход"}`)

	// Захват пачки (§13.1: перед сетью — sending) включает все четыре строки.
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 4 {
		t.Fatalf("захвачено %d, хочу 4", len(claimed))
	}
	if changed, deps, err := repo.MarkUnknown(ctx, id0, "context deadline exceeded"); err != nil || !changed {
		t.Fatalf("unknown не записан: changed=%v deps=%d err=%v", changed, deps, err)
	}
	// Зависимые — 2 (позиции 1 и 2), чужой turn не тронут.
	statuses := map[int64]string{}
	rows, err := pool.Query(ctx, `SELECT id, status FROM delivery_outbox`)
	if err != nil {
		t.Fatalf("статусы не читаются: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatalf("статус не читается: %v", err)
		}
		statuses[id] = st
	}
	rows.Close()
	if statuses[id0] != "unknown" {
		t.Fatalf("первый пузырь = %s, хочу unknown", statuses[id0])
	}
	if statuses[id1] != "cancelled" || statuses[id2] != "cancelled" {
		t.Fatalf("зависимые должны быть cancelled: %s, %s", statuses[id1], statuses[id2])
	}
	if statuses[other] != "sending" {
		t.Fatalf("чужой turn затронут: %s", statuses[other])
	}
}

func TestCancelTurnFrom(t *testing.T) {
	// §13.3: отмена оставшихся bubbles с позиции.
	pool := newTestPool(t)
	repo := NewOutboxRepo(pool)

	id0 := insertOutbox(t, pool, 50, 42, 0, `{"kind":"message","text":"ушёл"}`)
	insertOutbox(t, pool, 50, 42, 1, `{"kind":"message","text":"отменят"}`)
	id2 := insertOutbox(t, pool, 50, 42, 2, `{"kind":"message","text":"отменят"}`)

	// Позиция 0 уже sent — терминальна, отмена её не трогает.
	if _, err := pool.Exec(context.Background(),
		`UPDATE delivery_outbox SET status = 'sent' WHERE id = $1`, id0); err != nil {
		t.Fatalf("sent не выставлен: %v", err)
	}
	cancelled, err := repo.CancelTurnFrom(context.Background(), 50, 1, "тест отмены")
	if err != nil {
		t.Fatalf("отмена не удалась: %v", err)
	}
	if cancelled != 2 {
		t.Fatalf("отменено %d, хочу 2", cancelled)
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM delivery_outbox WHERE id = $1`, id2).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("пузырь 3 = %s (err=%v)", status, err)
	}
}

func TestRecoverStaleSending(t *testing.T) {
	// §12.2 «Outbox sending старого процесса — в unknown, не отправлять
	// вслепую»: зависшее sending → unknown + зависимые cancelled; свежее
	// sending живого процесса не трогается.
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	stale0 := insertOutbox(t, pool, 60, 42, 0, `{"kind":"message","text":"завис"}`)
	stale1 := insertOutbox(t, pool, 60, 42, 1, `{"kind":"message","text":"отменят"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'sending', attempts = 1,
		   updated_at = now() - interval '5 minutes'
		 WHERE id IN ($1, $2)`, stale0, stale1); err != nil {
		t.Fatalf("sending не выставлен: %v", err)
	}
	fresh := insertOutbox(t, pool, 61, 43, 0, `{"kind":"message","text":"живой"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'sending', attempts = 1 WHERE id = $1`, fresh); err != nil {
		t.Fatalf("свежий sending не выставлен: %v", err)
	}

	touched, err := repo.RecoverStaleSending(ctx, time.Minute)
	if err != nil {
		t.Fatalf("recovery не удался: %v", err)
	}
	if touched != 2 { // сам stale0 + отменённый stale1
		t.Fatalf("затронуто %d строк, хочу 2", touched)
	}
	statuses := map[int64]string{}
	rows, err := pool.Query(ctx, `SELECT id, status FROM delivery_outbox`)
	if err != nil {
		t.Fatalf("статусы не читаются: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatalf("статус не читается: %v", err)
		}
		statuses[id] = st
	}
	rows.Close()
	if statuses[stale0] != "unknown" || statuses[stale1] != "cancelled" {
		t.Fatalf("зависшее sending не восстановлено: %s, %s", statuses[stale0], statuses[stale1])
	}
	if statuses[fresh] != "sending" {
		t.Fatalf("свежее sending затронуто: %s", statuses[fresh])
	}
}
