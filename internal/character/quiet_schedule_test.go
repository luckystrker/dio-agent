// Юнит-тесты тихих часов расписания (этап 22.4, §14.3): тихий блок переносит
// инициативы на своё окончание, блок через полночь кончается в свою дату,
// идущие подряд тихие блоки склеиваются в одну тишину; днём и без расписания
// тишины нет.
package character

import (
	"testing"
	"time"
)

func TestQuietUntil(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	persona := Persona{Schedule: &sched}

	until, quiet := persona.QuietUntil(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc)
	if !quiet || !until.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, loc)) {
		t.Fatalf("тихо до 08:00, получено %v (quiet=%v)", until, quiet)
	}

	// Вечером тишина начинается в 23:00 и, склеившись с ночным блоком
	// 01:00–08:00, кончается только утром следующего дня.
	until, quiet = persona.QuietUntil(time.Date(2026, 11, 3, 23, 30, 0, 0, loc), loc)
	if !quiet || !until.Equal(time.Date(2026, 11, 4, 8, 0, 0, 0, loc)) {
		t.Fatalf("тихо до 08:00 следующего дня, получено %v (quiet=%v)", until, quiet)
	}

	// Пост-полуночная часть блока 23:00–01:00 кончается СЕГОДНЯ, а не через
	// сутки: иначе инициатива в 00:30 ждала бы до 01:00 завтрашнего дня.
	until, quiet = persona.QuietUntil(time.Date(2026, 11, 3, 0, 30, 0, 0, loc), loc)
	if !quiet || !until.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, loc)) {
		t.Fatalf("в 00:30 тихо до 08:00 сегодня, получено %v (quiet=%v)", until, quiet)
	}

	until, quiet = persona.QuietUntil(time.Date(2026, 11, 3, 12, 0, 0, 0, loc), loc)
	if quiet || !until.IsZero() {
		t.Fatalf("днём тишины нет, получено %v (quiet=%v)", until, quiet)
	}

	var bare Persona
	if _, quiet := bare.QuietUntil(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc); quiet {
		t.Fatal("без расписания тишины нет")
	}
}

// TestQuietUntilSingleBlock — одиночный тихий блок кончается в свою минуту;
// граница блока уже не тихая.
func TestQuietUntilSingleBlock(t *testing.T) {
	loc := moscow(t)
	sched := Schedule{
		{StartMinute: 22 * 60, EndMinute: 7 * 60, Quiet: true, Activity: "ночь"},
		{StartMinute: 7 * 60, EndMinute: 22 * 60, Activity: "день"},
	}
	persona := Persona{Schedule: &sched}
	until, quiet := persona.QuietUntil(time.Date(2026, 11, 3, 23, 0, 0, 0, loc), loc)
	if !quiet || !until.Equal(time.Date(2026, 11, 4, 7, 0, 0, 0, loc)) {
		t.Fatalf("ночь 22:00–07:00 кончается завтра в 07:00, получено %v", until)
	}
	if _, quiet := persona.QuietUntil(time.Date(2026, 11, 3, 7, 0, 0, 0, loc), loc); quiet {
		t.Fatal("на границе блока тишины уже нет")
	}
}
