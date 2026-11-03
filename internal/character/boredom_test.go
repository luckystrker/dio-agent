// Юнит-тесты шкалы скуки (этап 23.1): детерминированный рост от времени,
// насыщение, тихие блоки не растут, события сбрасывают. Время — параметр,
// поэтому тесты не зависят от системных часов.
package character

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestGrowBoredomLinearAndSaturates(t *testing.T) {
	start := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)

	// 3.5 часа бодрствования без расписания — половина шкалы.
	if got := GrowBoredom(0, start, start.Add(210*time.Minute), nil, nil); !near(got, 0.5) {
		t.Fatalf("за 3.5 ч скука = %v, хочу 0.5", got)
	}
	// За BoredomFullHours — полная скука, дальше не растёт.
	if got := GrowBoredom(0, start, start.Add(time.Duration(BoredomFullHours*float64(time.Hour))), nil, nil); !near(got, 1) {
		t.Fatalf("за %v ч скука = %v, хочу 1", BoredomFullHours, got)
	}
	if got := GrowBoredom(0.2, start, start.Add(100*time.Hour), nil, nil); got != 1 {
		t.Fatalf("потолок шкалы = %v, хочу 1", got)
	}
	// Время назад и нулевой интервал — без изменений.
	if got := GrowBoredom(0.3, start, start, nil, nil); got != 0.3 {
		t.Fatalf("нулевой интервал изменил скуку: %v", got)
	}
	if got := GrowBoredom(0.3, start, start.Add(-time.Hour), nil, nil); got != 0.3 {
		t.Fatalf("время назад изменило скуку: %v", got)
	}
}

func TestAwakeDurationSkipsQuietBlocks(t *testing.T) {
	loc := time.UTC
	schedule := Schedule{
		{StartMinute: 60, EndMinute: 8 * 60, Activity: "спит", Quiet: true},        // 01:00–08:00
		{StartMinute: 8 * 60, EndMinute: 23 * 60, Activity: "дела"},                // 08:00–23:00
		{StartMinute: 23 * 60, EndMinute: 60, Activity: "перед сном", Quiet: true}, // 23:00–01:00
	}

	// Ночь 23:30 → 09:00: бодрствование только 08:00–09:00.
	from := time.Date(2026, 11, 3, 23, 30, 0, 0, loc)
	to := time.Date(2026, 11, 4, 9, 0, 0, 0, loc)
	if got := AwakeDuration(from, to, &schedule, loc); got != time.Hour {
		t.Fatalf("ночное бодрствование = %v, хочу 1 ч", got)
	}
	// Вся ночь — ноль.
	if got := AwakeDuration(time.Date(2026, 11, 3, 23, 30, 0, 0, loc),
		time.Date(2026, 11, 4, 7, 0, 0, 0, loc), &schedule, loc); got != 0 {
		t.Fatalf("в тихий блок скука выросла на %v", got)
	}

	// Скука в тихом блоке не растёт.
	if got := GrowBoredom(0.4, from, to, &schedule, loc); !near(got, 0.4+BoredomGrowthPerHour) {
		t.Fatalf("рост за ночь = %v, хочу %v", got, 0.4+BoredomGrowthPerHour)
	}

	// День целиком бодрствования — обычный линейный рост.
	dayFrom := time.Date(2026, 11, 4, 8, 0, 0, 0, loc)
	if got := AwakeDuration(dayFrom, dayFrom.Add(3*time.Hour), &schedule, loc); got != 3*time.Hour {
		t.Fatalf("дневное бодрствование = %v, хочу 3 ч", got)
	}
	// Весь дневной блок 08:00–23:00 — 15 часов: конец считается от начала
	// блока, а не от полуночи (регрессия «блок кончается в 15:00»).
	if got := AwakeDuration(dayFrom, time.Date(2026, 11, 4, 23, 0, 0, 0, loc), &schedule, loc); got != 15*time.Hour {
		t.Fatalf("дневной блок = %v, хочу 15 ч", got)
	}

	// Тихий блок через полночь тоже не считается (23:00–01:00).
	if got := AwakeDuration(time.Date(2026, 11, 3, 23, 0, 0, 0, loc),
		time.Date(2026, 11, 4, 1, 0, 0, 0, loc), &schedule, loc); got != 0 {
		t.Fatalf("блок через полночь посчитан как бодрствование: %v", got)
	}

	// Бодрствующий блок через полночь (18:00–02:00) — 8 часов, а не 0.
	wrap := Schedule{
		{StartMinute: 2 * 60, EndMinute: 18 * 60, Activity: "спит", Quiet: true},
		{StartMinute: 18 * 60, EndMinute: 2 * 60, Activity: "гуляет"},
	}
	if got := AwakeDuration(time.Date(2026, 11, 3, 18, 0, 0, 0, loc),
		time.Date(2026, 11, 4, 2, 0, 0, 0, loc), &wrap, loc); got != 8*time.Hour {
		t.Fatalf("бодрствование через полночь = %v, хочу 8 ч", got)
	}
}

// TestAwakeDurationDST — день перевода стрелок: блок задан стенными часами,
// поэтому 01:00–08:00 в Europe/Berlin весной — 6 физических часов, а не 7.
func TestAwakeDurationDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("зона Europe/Berlin недоступна: %v", err)
	}
	schedule := Schedule{
		{StartMinute: 0, EndMinute: 60, Activity: "спит", Quiet: true},
		{StartMinute: 60, EndMinute: 8 * 60, Activity: "дела"},
		{StartMinute: 8 * 60, EndMinute: 24 * 60, Activity: "спит", Quiet: true},
	}
	// 29.03.2026 — переход на летнее время (02:00 → 03:00).
	from := time.Date(2026, 3, 29, 1, 0, 0, 0, loc)
	to := time.Date(2026, 3, 29, 9, 0, 0, 0, loc)
	if got := AwakeDuration(from, to, &schedule, loc); got != 6*time.Hour {
		t.Fatalf("бодрствование в день перевода стрелок = %v, хочу 6 ч", got)
	}
}

func TestBoredomAfterEvent(t *testing.T) {
	if got := boredomAfterEvent(0.8, BoredomConversation); got != 0 {
		t.Fatalf("разговорный ход сбрасывает скуку в 0, получено %v", got)
	}
	if got := boredomAfterEvent(0.9, BoredomExplore); !near(got, 0.4) {
		t.Fatalf("находка сбрасывает на %v, получено %v", BoredomExploreRelief, got)
	}
	if got := boredomAfterEvent(0.2, BoredomExplore); got != 0 {
		t.Fatalf("скудка не уходит ниже нуля: %v", got)
	}
}
