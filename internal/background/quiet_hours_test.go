// Юнит-тесты тихих часов (этап 22.4, §14.3): в тихом блоке расписания
// инициативы (check_in/share/reply_later вне живого ответа) не исполняются, а
// переносятся на конец блока; explore — внутреннее действие, тишиной не
// блокируется; срок, истекающий внутри тишины, отменяет намерение сразу.
package background

import (
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
)

// quietSchedule — сутки: тихо 01:00–08:00 (и 23:00–01:00 через полночь),
// остальное — бодрствование. Достаточно для выбора блока по времени.
func quietSchedule() character.Schedule {
	return character.Schedule{
		{StartMinute: 60, EndMinute: 8 * 60, Activity: "спит", Quiet: true},
		{StartMinute: 8 * 60, EndMinute: 23 * 60, Activity: "дела"},
		{StartMinute: 23 * 60, EndMinute: 60, Activity: "перед сном", Quiet: true},
	}
}

func quietTestScheduler(at time.Time, loc *time.Location, sched *character.Schedule) *Scheduler {
	return &Scheduler{
		persona:  character.Persona{Schedule: sched},
		location: loc,
		now:      func() time.Time { return at },
	}
}

func moscowLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона недоступна: %v", err)
	}
	return loc
}

func TestQuietDecisionInitiativeKinds(t *testing.T) {
	loc := moscowLoc(t)
	sched := quietSchedule()
	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	s := quietTestScheduler(night, loc, &sched)

	for _, kind := range []string{KindCheckIn, KindShare, KindReplyLater} {
		decision := s.quietDecision(Intention{Kind: kind})
		if !decision.Defer || decision.Cancel {
			t.Fatalf("%s ночью должна ждать конца тихого блока: %+v", kind, decision)
		}
		if want := time.Date(2026, 11, 3, 8, 0, 0, 0, loc); !decision.Until.Equal(want) {
			t.Fatalf("%s: notBefore = %v, хочу %v", kind, decision.Until, want)
		}
	}

	// explore — внутреннее действие: тишина его не блокирует.
	if decision := s.quietDecision(Intention{Kind: KindExplore}); decision.Defer || decision.Cancel {
		t.Fatalf("explore не должен откладываться тихими часами: %+v", decision)
	}
}

func TestQuietDecisionOutsideQuiet(t *testing.T) {
	loc := moscowLoc(t)
	sched := quietSchedule()
	day := time.Date(2026, 11, 3, 12, 0, 0, 0, loc)
	s := quietTestScheduler(day, loc, &sched)
	if decision := s.quietDecision(Intention{Kind: KindCheckIn}); decision.Defer || decision.Cancel {
		t.Fatalf("днём инициатива не откладывается: %+v", decision)
	}
}

// TestQuietDecisionCrossesMidnight — тихий блок через полночь и следующий за
// ним тихий блок — одна тишина: инициатива ждёт утра, а не стыка в 01:00.
func TestQuietDecisionCrossesMidnight(t *testing.T) {
	loc := moscowLoc(t)
	sched := quietSchedule()
	late := time.Date(2026, 11, 3, 23, 30, 0, 0, loc)
	s := quietTestScheduler(late, loc, &sched)
	decision := s.quietDecision(Intention{Kind: KindShare})
	if !decision.Defer {
		t.Fatal("поздно вечером инициатива ждёт конца тихого блока")
	}
	if want := time.Date(2026, 11, 4, 8, 0, 0, 0, loc); !decision.Until.Equal(want) {
		t.Fatalf("notBefore = %v, хочу %v", decision.Until, want)
	}
}

// TestQuietDecisionExpiresInQuiet — срок истекает внутри тишины: намерение
// отменяется сразу, а не «откладывается» в никуда (claim его не возьмёт).
func TestQuietDecisionExpiresInQuiet(t *testing.T) {
	loc := moscowLoc(t)
	sched := quietSchedule()
	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	s := quietTestScheduler(night, loc, &sched)
	expires := time.Date(2026, 11, 3, 7, 0, 0, 0, loc)
	decision := s.quietDecision(Intention{Kind: KindCheckIn, ExpiresAt: &expires})
	if !decision.Cancel || decision.Defer {
		t.Fatalf("истекающее в тишину намерение отменяется: %+v", decision)
	}
	// Срок после конца тишины — обычный перенос.
	late := time.Date(2026, 11, 3, 20, 0, 0, 0, loc)
	if got := s.quietDecision(Intention{Kind: KindCheckIn, ExpiresAt: &late}); !got.Defer {
		t.Fatalf("срок после тишины — перенос, получено %+v", got)
	}
}

// TestQuietDecisionNoSchedule — без расписания прежнее поведение: инициативы
// не откладываются.
func TestQuietDecisionNoSchedule(t *testing.T) {
	loc := time.UTC
	s := quietTestScheduler(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc, nil)
	if decision := s.quietDecision(Intention{Kind: KindCheckIn}); decision.Defer || decision.Cancel {
		t.Fatalf("без расписания тихих часов нет: %+v", decision)
	}
}
