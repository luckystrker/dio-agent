//go:build db

// БД-тест проводки тихих часов (этап 22.4): в тихом блоке такт не исполняет
// инициативу — модель не вызывается, ход инициативы не создаётся, намерение
// остаётся pending с notBefore = конец тишины. Проверяется именно путь
// runIntention, а не только чистое решение: переезд гейта или потеря пояса при
// сборке планировщика должны валить этот тест.
package background

import (
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
)

func TestQuietHoursDeferInitiativeDB(t *testing.T) {
	f, ctx := newFixture(t)
	loc := moscowLoc(t)
	sched := quietSchedule()
	f.sched.persona.Schedule = &sched
	f.sched.location = loc

	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	f.setNow(night)
	// Чат приватный и тихий (последнее событие два часа назад): гейты
	// актуальности проходят, дело доходит до тишины.
	f.seedPrivateChat(t, ctx, "как дела?", night.Add(-2*time.Hour))
	model := f.withModel(t) // модель в тихий блок вызываться не должна

	id := f.createIntention(t, ctx, KindCheckIn, "как ты там", -1, bgChatID)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}

	if got := f.intentionStatus(t, ctx, id); got != "pending" {
		t.Fatalf("статус намерения = %q, хочу pending (тихие часы)", got)
	}
	want := time.Date(2026, 11, 3, 8, 0, 0, 0, loc)
	if got := f.intentionNotBefore(t, ctx, id); got.Sub(want) < -time.Second || got.Sub(want) > time.Second {
		t.Fatalf("notBefore = %v, хочу ~%v", got, want)
	}
	if calls := len(model.Calls()); calls != 0 {
		t.Fatalf("модель вызвана %d раз в тихий блок", calls)
	}
	var turns int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM turns WHERE chat_id = $1`, bgChatID).Scan(&turns); err != nil {
		t.Fatalf("ходы не читаются: %v", err)
	}
	if turns != 0 {
		t.Fatalf("ночью создан ход инициативы: %d", turns)
	}

	// Днём та же инициатива исполняется обычным путём: гейт тишины не залип.
	day := time.Date(2026, 11, 3, 12, 0, 0, 0, loc)
	f.setNow(day)
	f.seedPrivateChat(t, ctx, "привет", day.Add(-2*time.Hour))
	f.withModel(t, &events.ModelResponse{Message: &events.ModelMessage{Content: initiativeTurnJSON}})
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("дневной такт не удался: %v", err)
	}
	if got := f.intentionStatus(t, ctx, id); got == "pending" || got == "running" {
		t.Fatalf("днём намерение не исполнилось: статус %q", got)
	}
}
