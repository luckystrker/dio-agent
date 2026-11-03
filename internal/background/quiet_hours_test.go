// Юнит-тесты тихих часов (этап 22.4, §14.3): в тихом блоке расписания
// инициативы (check_in/share/reply_later вне живого ответа) не исполняются, а
// переносятся на конец блока; explore — внутреннее действие, тишиной не
// блокируется.
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

func TestQuietDeferralInitiativeKinds(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона недоступна: %v", err)
	}
	sched := quietSchedule()
	night := time.Date(2026, 11, 3, 3, 0, 0, 0, loc)
	s := quietTestScheduler(night, loc, &sched)

	for _, kind := range []string{KindCheckIn, KindShare, KindReplyLater} {
		until, deferred := s.quietDeferral(Intention{Kind: kind})
		if !deferred {
			t.Fatalf("%s ночью должна ждать конца тихого блока", kind)
		}
		if want := time.Date(2026, 11, 3, 8, 0, 0, 0, loc); !until.Equal(want) {
			t.Fatalf("%s: notBefore = %v, хочу %v", kind, until, want)
		}
	}

	// explore — внутреннее действие: тишина его не блокирует.
	if _, deferred := s.quietDeferral(Intention{Kind: KindExplore}); deferred {
		t.Fatal("explore не должен откладываться тихими часами")
	}
}

func TestQuietDeferralOutsideQuiet(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона недоступна: %v", err)
	}
	sched := quietSchedule()
	day := time.Date(2026, 11, 3, 12, 0, 0, 0, loc)
	s := quietTestScheduler(day, loc, &sched)
	if _, deferred := s.quietDeferral(Intention{Kind: KindCheckIn}); deferred {
		t.Fatal("днём инициатива не откладывается")
	}
}

// TestQuietDeferralCrossesMidnight — тихий блок через полночь переносит
// инициативу на следующее утро, а не на сегодняшнее 00:00 (уже прошло).
func TestQuietDeferralCrossesMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона недоступна: %v", err)
	}
	sched := quietSchedule()
	late := time.Date(2026, 11, 3, 23, 30, 0, 0, loc)
	s := quietTestScheduler(late, loc, &sched)
	until, deferred := s.quietDeferral(Intention{Kind: KindShare})
	if !deferred {
		t.Fatal("поздно вечером инициатива ждёт конца тихого блока")
	}
	if want := time.Date(2026, 11, 4, 1, 0, 0, 0, loc); !until.Equal(want) {
		t.Fatalf("notBefore = %v, хочу %v", until, want)
	}
}

// TestQuietDeferralNoSchedule — без расписания прежнее поведение: инициативы
// не откладываются.
func TestQuietDeferralNoSchedule(t *testing.T) {
	loc := time.UTC
	s := quietTestScheduler(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc, nil)
	if _, deferred := s.quietDeferral(Intention{Kind: KindCheckIn}); deferred {
		t.Fatal("без расписания тихих часов нет")
	}
}
