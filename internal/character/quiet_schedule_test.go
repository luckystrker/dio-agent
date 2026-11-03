// Юнит-тесты тихих часов расписания (этап 22.4, §14.3): тихий блок переносит
// инициативы на своё окончание, блок через полночь кончается на следующий
// день; днём и без расписания тишины нет.
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

	until, quiet = persona.QuietUntil(time.Date(2026, 11, 3, 23, 30, 0, 0, loc), loc)
	if !quiet || !until.Equal(time.Date(2026, 11, 4, 1, 0, 0, 0, loc)) {
		t.Fatalf("тихо до 01:00 следующего дня, получено %v (quiet=%v)", until, quiet)
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
