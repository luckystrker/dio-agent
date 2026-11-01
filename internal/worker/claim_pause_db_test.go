//go:build db

// БД-тест паузы очереди (16.7): ход на локальной notBefore-паузе (отложен до
// завтра по бюджету) не выбирается claim-ом до срока; после истечения срока
// пауза снимается и ход доезжает. TestWorkerBudgetExhaustedDefersToNextDay
// проверяет границу суток только косвенно (пауза снимается вручную, как
// рестарт) — здесь проверена сама механика истечения в claimNext.
package worker

import (
	"context"
	"log"
	"testing"
	"time"
)

// TestWorkerClaimSkipsPausedTurnUntilExpiry — активная пауза пропускается без
// попытки; истёкшая — снимается, ход забирается (первой попыткой, attempts=1).
func TestWorkerClaimSkipsPausedTurnUntilExpiry(t *testing.T) {
	pool := newWorkerPool(t)
	ctx := context.Background()

	turnID := seedTurn(t, pool, 3005, "private", "ход на паузе до завтра")
	w := New(Deps{Pool: pool, Runner: &fakeRunner{pool: pool},
		Logger: log.New(discardWriter{}, "", 0)},
		Options{PollInterval: 10 * time.Millisecond})

	// Пауза активна (как «отложено до завтра 00:05» по бюджету): claim не берёт
	// ход и не тратит попытку.
	w.notBeforeMu.Lock()
	w.notBefore[turnID] = time.Now().Add(time.Hour)
	w.notBeforeMu.Unlock()
	got, err := w.claimNext(ctx)
	if err != nil {
		t.Fatalf("claimNext: %v", err)
	}
	if got != nil {
		t.Fatalf("ход на активной паузе не должен выбираться, взяли ход %d", got.ID)
	}
	assertTurnState(t, pool, ctx, turnID, "created", 0)

	// Срок паузы прошёл (наступила граница суток): claim берёт ход первой
	// попыткой и снимает истёкшую запись паузы.
	w.notBeforeMu.Lock()
	w.notBefore[turnID] = time.Now().Add(-time.Minute)
	w.notBeforeMu.Unlock()
	got, err = w.claimNext(ctx)
	if err != nil {
		t.Fatalf("claimNext: %v", err)
	}
	if got == nil || got.ID != turnID {
		t.Fatalf("ход после срока паузы должен быть взят, got=%v", got)
	}
	if got.Attempt != 1 {
		t.Fatalf("попытка захвата = %d, хочу 1", got.Attempt)
	}
	w.notBeforeMu.Lock()
	_, still := w.notBefore[turnID]
	w.notBeforeMu.Unlock()
	if still {
		t.Fatal("истёкшая пауза должна сниматься при выборе хода")
	}
}
