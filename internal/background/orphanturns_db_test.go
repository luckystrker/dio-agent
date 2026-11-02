//go:build db

// БД-тесты ходов-сирот инициатив (блок 18.7): ход создаётся до генерации
// (status created); истечение или отмена намерения обязана пометить его
// skipped — иначе он навсегда оседает created без событий и worker его не
// берёт. Страховка в такте подбирает сирот, оставшихся любым путём.
package background

import (
	"context"
	"testing"
)

// turnStatus — текущий статус хода.
func (f *fixture) turnStatus(t *testing.T, ctx context.Context, id int64) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(ctx,
		`SELECT status FROM turns WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("ход %d не читается: %v", id, err)
	}
	return status
}

// TestExpiredIntentionSkipsOrphanTurn — сценарий сироты: намерение забрано в
// running, ход инициативы создан (до генерации), процесс упал; намерение
// вернулось в pending, срок истёк. Такт отменяет намерение — ход обязан стать
// skipped, а не висеть created.
func TestExpiredIntentionSkipsOrphanTurn(t *testing.T) {
	f, ctx := newFixture(t)
	id := f.createIntention(t, ctx, KindReplyLater, "тема сироты", -6, bgChatID)

	// Исполнение началось: claim → running, ход создан до генерации.
	claimed, err := f.intentions.ClaimDue(ctx, 1)
	if err != nil {
		t.Fatalf("claim не удался: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claim вернул %d намерений, хочу только %d", len(claimed), id)
	}
	turnID, err := f.sched.createInitiativeTurn(ctx, claimed[0])
	if err != nil {
		t.Fatalf("ход инициативы не создан: %v", err)
	}

	// Краш процесса: восстановление возвращает running в pending (§12.2).
	if _, err := f.intentions.ResetRunning(ctx); err != nil {
		t.Fatalf("running не сброшен: %v", err)
	}
	// Срок истёк — следующая проверка отменяет намерение cleanup'ом (§14.2).
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatalf("expires_at не сдвинут: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if st := f.intentionStatus(t, ctx, id); st != "cancelled" {
		t.Fatalf("просроченное намерение должно быть cancelled, статус %s", st)
	}
	if st := f.turnStatus(t, ctx, turnID); st != "skipped" {
		t.Fatalf("ход истёкшего намерения обязан стать skipped, а не висеть created (блок 18.7), статус %s", st)
	}
}

// TestOrphanTurnSafetyNet — страховка такта: created-ход без событий, чьё
// намерение уже терминально (отменено раньше, ход остался), помечается skipped
// самим тактом — сирота не живёт вечно ни при каком пути появления.
func TestOrphanTurnSafetyNet(t *testing.T) {
	f, ctx := newFixture(t)
	id := f.createIntention(t, ctx, KindShare, "находка-сирота", -6, bgChatID)

	var turnID int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, bgChatID).Scan(&turnID); err != nil {
		t.Fatalf("ход не создан: %v", err)
	}
	// Намерение уже терминально (отменено в прошлом прогоне), ход-сирота остался.
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'cancelled', related_turn_id = $2, updated_at = now() WHERE id = $1`,
		id, turnID); err != nil {
		t.Fatalf("намерение не отменено: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if st := f.turnStatus(t, ctx, turnID); st != "skipped" {
		t.Fatalf("страховка такта обязана помечать сироту терминального намерения skipped (блок 18.7), статус %s", st)
	}
}

// TestOrphanTurnSafetyNetKeepsLiveTurns — страховка не трогает ходы прямых
// обращений (created с событиями) и created-ходы живого намерения.
func TestOrphanTurnSafetyNetKeepsLiveTurns(t *testing.T) {
	f, ctx := newFixture(t)
	liveID := f.createIntention(t, ctx, KindReplyLater, "живая тема", -6, bgChatID)

	var initiativeTurn int64
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, bgChatID).Scan(&initiativeTurn); err != nil {
		t.Fatalf("ход инициативы не создан: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET related_turn_id = $2 WHERE id = $1`, liveID, initiativeTurn); err != nil {
		t.Fatalf("ход не привязан к намерению: %v", err)
	}
	directTurn := f.seedDirectTurnPending(t, ctx)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if st := f.turnStatus(t, ctx, initiativeTurn); st != "created" {
		t.Fatalf("ход живого намерения не должен помечаться skipped, статус %s", st)
	}
	if st := f.turnStatus(t, ctx, directTurn); st != "created" {
		t.Fatalf("прямой ход с событиями не должен помечаться skipped, статус %s", st)
	}
}
