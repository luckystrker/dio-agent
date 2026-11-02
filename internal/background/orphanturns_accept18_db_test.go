//go:build db

// Дополнительные БД-проверки приёмки блока 18.7 (тестировщик): сценарии, не
// покрытые тестами разработчика, — прямая отмена намерения с созданным ходом
// (путь Cancel, не только ExpireOverdue) и однократность события turn_skipped
// у страховки такта.
package background

import (
	"context"
	"fmt"
	"testing"
)

// turnSkippedEvents — сколько событий turn_skipped записано в журнал хода.
func (f *fixture) turnSkippedEvents(t *testing.T, ctx context.Context, turnID int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM event_streams WHERE stream_id = $1 AND type = 'turn_skipped'`,
		fmt.Sprintf("turn:%d", turnID)).Scan(&n); err != nil {
		t.Fatalf("журнал хода %d не читается: %v", turnID, err)
	}
	return n
}

// TestCancelMarksRelatedTurnSkipped — путь Cancel (гейт отменяет уже
// начатое намерение): связанный created-ход помечается skipped той же
// транзакцией отмены; повторная отмена безвредна (блок 18.7).
func TestCancelMarksRelatedTurnSkipped(t *testing.T) {
	f, ctx := newFixture(t)
	id := f.createIntention(t, ctx, KindShare, "отмена с ходом", -6, bgChatID)

	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, bgChatID).Scan(&turnID); err != nil {
		t.Fatalf("ход не создан: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET related_turn_id = $2, status = 'running' WHERE id = $1`, id, turnID); err != nil {
		t.Fatalf("намерение не связано с ходом: %v", err)
	}

	ok, err := f.intentions.Cancel(ctx, id, "тема закрылась")
	if err != nil || !ok {
		t.Fatalf("отмена обязана проходить: ok=%v err=%v", ok, err)
	}
	if st := f.intentionStatus(t, ctx, id); st != "cancelled" {
		t.Fatalf("намерение обязано стать cancelled, статус %s", st)
	}
	if st := f.turnStatus(t, ctx, turnID); st != "skipped" {
		t.Fatalf("ход отменённого намерения обязан стать skipped (блок 18.7), статус %s", st)
	}

	// Повторная отмена: нечего отменять — не ошибка, ход не трогает.
	ok2, err2 := f.intentions.Cancel(ctx, id, "повторно")
	if err2 != nil || ok2 {
		t.Fatalf("повторная отмена: ok=%v err=%v, хочу false/nil", ok2, err2)
	}
	if st := f.turnStatus(t, ctx, turnID); st != "skipped" {
		t.Fatalf("повторная отмена не должна трогать ход, статус %s", st)
	}
}

// TestOrphanSafetyNetWritesTurnSkippedOnce — страховка пишет терминальное
// событие turn_skipped в журнал хода (§12.1, инвариант 5) ровно один раз:
// повторный такт не дублирует событие и не трогает уже skipped ход.
func TestOrphanSafetyNetWritesTurnSkippedOnce(t *testing.T) {
	f, ctx := newFixture(t)
	id := f.createIntention(t, ctx, KindShare, "сирота с журналом", -6, bgChatID)

	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, bgChatID).Scan(&turnID); err != nil {
		t.Fatalf("ход не создан: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'cancelled', related_turn_id = $2 WHERE id = $1`, id, turnID); err != nil {
		t.Fatalf("намерение не отменено: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if st := f.turnStatus(t, ctx, turnID); st != "skipped" {
		t.Fatalf("страховка обязана помечать сироту skipped (блок 18.7), статус %s", st)
	}
	if n := f.turnSkippedEvents(t, ctx, turnID); n != 1 {
		t.Fatalf("страховка обязана писать ровно одно событие turn_skipped (§12.1), записано %d", n)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("повторный такт не удался: %v", err)
	}
	if n := f.turnSkippedEvents(t, ctx, turnID); n != 1 {
		t.Fatalf("повторный такт не должен дублировать turn_skipped, записано %d", n)
	}
}
