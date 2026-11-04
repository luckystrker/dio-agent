//go:build db

// БД-тесты очереди аватарок (34.2): первый ход ставит проверку, повторные — не
// чаще суточного окна; claim берёт строку один раз (lease), Complete фиксирует
// file_unique_id и срок следующей проверки.
package character

import (
	"context"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/avatar"
)

func TestTurnEnqueuesAvatarCheckDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(1201)
	f.seedTurn(t, ctx, turnID, "привет, как дела?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: validFinalJSON()}})

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет, как дела?"}}})
	if err != nil || !out.Apply.Applied {
		t.Fatalf("ход не применён: %+v err=%v", out, err)
	}

	var due bool
	if err := f.pool.QueryRow(ctx, `
		SELECT next_check_at <= now() FROM person_avatars
		WHERE user_id = 777 AND scope_id = $1`, testScopeID).Scan(&due); err != nil {
		t.Fatalf("очередь аватарки не создана: %v", err)
	}
	if !due {
		t.Fatalf("первый разговор должен сразу ставить проверку аватарки")
	}

	// Проверка выполнена (сутки не прошли): повторный ход не сдвигает срок и не
	// дублирует строку — аватарку на обычных ходах не качаем.
	if err := avatar.Complete(ctx, f.pool, 777, testScopeID, "u1", true, "кот"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var next time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT next_check_at FROM person_avatars WHERE user_id = 777 AND scope_id = $1`,
		testScopeID).Scan(&next); err != nil {
		t.Fatalf("срок не читается: %v", err)
	}
	if err := avatar.Enqueue(ctx, f.pool, 777, testScopeID, testChatID, "Коля"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var after time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT next_check_at FROM person_avatars WHERE user_id = 777 AND scope_id = $1`,
		testScopeID).Scan(&after); err != nil {
		t.Fatalf("срок не читается: %v", err)
	}
	if !after.Equal(next) {
		t.Fatalf("повторный ход сдвинул суточный срок: было %v, стало %v", next, after)
	}

	// Срок истёк — ход снова ставит проверку.
	if _, err := f.pool.Exec(ctx, `
		UPDATE person_avatars SET next_check_at = now() - interval '1 minute'
		WHERE user_id = 777 AND scope_id = $1`, testScopeID); err != nil {
		t.Fatalf("срок не сдвинут: %v", err)
	}
	if err := avatar.Enqueue(ctx, f.pool, 777, testScopeID, testChatID, "Коля"); err != nil {
		t.Fatalf("Enqueue после срока: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT next_check_at <= now() FROM person_avatars WHERE user_id = 777 AND scope_id = $1`,
		testScopeID).Scan(&due); err != nil {
		t.Fatalf("срок не читается: %v", err)
	}
	if !due {
		t.Fatalf("после суток проверка должна снова попасть в очередь")
	}
}

func TestAvatarClaimLeaseDB(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	if err := avatar.Enqueue(ctx, pool, 777, testScopeID, testChatID, "Коля"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	checks, err := avatar.ClaimDue(ctx, pool, 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(checks) != 1 || checks[0].UserID != 777 || checks[0].ScopeID != testScopeID || checks[0].ActorName != "Коля" {
		t.Fatalf("claim: %+v", checks)
	}
	again, err := avatar.ClaimDue(ctx, pool, 10)
	if err != nil {
		t.Fatalf("повторный ClaimDue: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("lease не удержал строку: %+v", again)
	}
	if err := avatar.Complete(ctx, pool, 777, testScopeID, "u1", true, "кот"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var (
		unique    string
		visible   bool
		checkedAt *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT file_unique_id, visible, checked_at FROM person_avatars
		WHERE user_id = 777 AND scope_id = $1`, testScopeID).Scan(&unique, &visible, &checkedAt); err != nil {
		t.Fatalf("результат не читается: %v", err)
	}
	if unique != "u1" || !visible || checkedAt == nil {
		t.Fatalf("результат: unique=%q visible=%v checked=%v", unique, visible, checkedAt)
	}
}
