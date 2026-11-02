//go:build db

// БД-тесты outbox §13.1–13.2: захват по порядку чата, отсутствие дублей
// захвата, условная запись sent, unknown-политика с отменой зависимых,
// восстановление sending после сбоя процесса (§12.2).
package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// insertOutbox вставляет строку outbox напрямую (создание строк — зона 9.4;
// здесь важна машина статусов).
func insertOutbox(t *testing.T, pool *runtime.Pool, turnID, chatID int64, position int, payload string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO delivery_outbox (turn_id, position, chat_id, payload) VALUES ($1, $2, $3, $4::jsonb) RETURNING id`,
		turnID, position, chatID, payload).Scan(&id); err != nil {
		t.Fatalf("outbox-строка не вставлена: %v", err)
	}
	return id
}

// insertOutboxSent — уже доставленная строка (позиция 0 первого пузыря).
func insertOutboxSent(t *testing.T, pool *runtime.Pool, turnID, chatID int64, position int, payload string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, telegram_message_id)
		 VALUES ($1, $2, $3, $4::jsonb, 'sent', 500) RETURNING id`,
		turnID, position, chatID, payload).Scan(&id); err != nil {
		t.Fatalf("sent-outbox-строка не вставлена: %v", err)
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
	// §13.1: перед сетью sending; порядок внутри чата (chat, turn, position).
	// После 17.7 клейм берёт только «головы» очередей чатов: поздний пузырь
	// не обгоняет ранний, следующий уходит после исхода предыдущего.
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	idB1 := insertOutbox(t, pool, 11, -5, 0, `{"kind":"message","text":"группа"}`)
	idA0 := insertOutbox(t, pool, 10, 42, 0, `{"kind":"message","text":"первый"}`)
	idA1 := insertOutbox(t, pool, 10, 42, 1, `{"kind":"message","text":"второй"}`)

	// Первый захват — по голове каждой очереди: группа -5 раньше лички 42
	// (числовой порядок chat_id, паритет сортировки TS); пузырь 1 лички
	// держит пузырь 2 того же turn.
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 2 {
		t.Fatalf("захвачено %d, хочу 2 головы очередей: %+v", len(claimed), claimed)
	}
	if claimed[0].ID != idB1 || claimed[1].ID != idA0 {
		t.Fatalf("порядок доставки нарушен: %+v", claimed)
	}
	for _, item := range claimed {
		if item.Status != OutboxSending || item.Attempts != 1 {
			t.Fatalf("захваченное должно быть sending/1: %+v", item)
		}
	}

	// Повторный захват пуст: строки в sending не перезахватываются, а
	// зависимый пузырь 2 ждёт исхода пузыря 1.
	if again := mustClaim(t, repo, 10); len(again) != 0 {
		t.Fatalf("второй захват выдал те же/очередные строки: %+v", again)
	}

	// Первый пузырь лички ушёл — пузырь 2 освобождён и уходит следующим.
	if _, err := pool.Exec(ctx, `UPDATE delivery_outbox SET status = 'sent' WHERE id = $1`, idA0); err != nil {
		t.Fatalf("sent не выставлен: %v", err)
	}
	claimed = mustClaim(t, repo, 10)
	if len(claimed) != 1 || claimed[0].ID != idA1 {
		t.Fatalf("после sent первого должен уходить второй: %+v", claimed)
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

// TestClaimDueEarlyBubbleBlocksLater — 17.7: более ранний нетерминальный
// пузырь чата (pending/sending/retry_wait) держит поздние: без этого поздний
// отправлялся первым и ответ читался навыворот («…и поэтому» до «Привет!»).
// Фильтр видимости not_before на блокировку не влияет: ранняя строка во
// флуд-паузе с будущим not_before всё равно старше и не даёт обгонять себя.
func TestClaimDueEarlyBubbleBlocksLater(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	earlyID := insertOutbox(t, pool, 70, 42, 0, `{"kind":"message","text":"Привет!"}`)
	lateID := insertOutbox(t, pool, 71, 42, 0, `{"kind":"message","text":"…и поэтому"}`)
	// Ранний пузырь — во флуд-паузе (retry_wait, not_before в будущем).
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'retry_wait', not_before = now() + interval '1 hour' WHERE id = $1`, earlyID); err != nil {
		t.Fatalf("retry_wait не выставлен: %v", err)
	}
	if claimed := mustClaim(t, repo, 10); len(claimed) != 0 {
		t.Fatalf("поздний пузырь обогнал ранний retry_wait: %+v", claimed)
	}

	// Ранняя строка восстановилась (not_before наступил) — уходит первой;
	// поздний ждёт её исхода, пока она sending.
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET not_before = now() - interval '1 second' WHERE id = $1`, earlyID); err != nil {
		t.Fatalf("not_before не передвинут: %v", err)
	}
	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 || claimed[0].ID != earlyID {
		t.Fatalf("первым должен уходить ранний пузырь: %+v", claimed)
	}
	if again := mustClaim(t, repo, 10); len(again) != 0 {
		t.Fatalf("поздний обогнал отправляемый ранний (sending): %+v", again)
	}

	// Ранний ушёл (терминальный sent) — поздний свободен: порядок соблюдён.
	if _, err := pool.Exec(ctx, `UPDATE delivery_outbox SET status = 'sent' WHERE id = $1`, earlyID); err != nil {
		t.Fatalf("sent не выставлен: %v", err)
	}
	claimed = mustClaim(t, repo, 10)
	if len(claimed) != 1 || claimed[0].ID != lateID {
		t.Fatalf("после раннего должен уходить поздний: %+v", claimed)
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

	// Все четыре строки — sending (после 17.7 клейм берёт голову очереди
	// чата, а здесь проверяется машина unknown, а не порядок клейма).
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'sending', attempts = 1`); err != nil {
		t.Fatalf("sending не выставлен: %v", err)
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

// TestClaimDueBlockedChatDoesNotBlockOthers — приёмка 17.7: блокировка
// «раньше голова очереди» — строго в пределах чата: флуд-пауза одного чата
// не должна морить голом соседние (EXISTS-фильтр коррелирован по chat_id).
// Терминальный failed ранней строки поздних не держит — очередь движется.
func TestClaimDueBlockedChatDoesNotBlockOthers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := NewOutboxRepo(pool)

	// Чат 42: ранняя строка во флуд-паузе, поздняя ждёт.
	early := insertOutbox(t, pool, 80, 42, 0, `{"kind":"message","text":"ранний"}`)
	insertOutbox(t, pool, 81, 42, 0, `{"kind":"message","text":"поздний"}`)
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'retry_wait', not_before = now() + interval '1 hour' WHERE id = $1`, early); err != nil {
		t.Fatalf("retry_wait не выставлен: %v", err)
	}
	// Чат 43: обычная готовая строка — должна уйти, пока чат 42 в паузе.
	other := insertOutbox(t, pool, 82, 43, 0, `{"kind":"message","text":"сосед"}`)

	claimed := mustClaim(t, repo, 10)
	if len(claimed) != 1 || claimed[0].ID != other {
		t.Fatalf("заблокированный чат 42 должен держать только свои строки, а не соседний чат: %+v", claimed)
	}

	// Ранняя строка чата 42 исчерпала попытки (failed — терминальный):
	// поздняя строка освобождается.
	if _, err := pool.Exec(ctx,
		`UPDATE delivery_outbox SET status = 'failed' WHERE id = $1`, early); err != nil {
		t.Fatalf("failed не выставлен: %v", err)
	}
	claimed = mustClaim(t, repo, 10)
	if len(claimed) != 1 {
		t.Fatalf("после failed ранней строки поздняя должна быть доступна: %+v", claimed)
	}
	if claimed[0].ChatID != 42 {
		t.Fatalf("должна уйти строка чата 42: %+v", claimed[0])
	}
}
