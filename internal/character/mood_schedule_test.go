// Юнит-тесты дрейфа настроения к блоку расписания (этап 22.2): цель затухания —
// блок текущего часа (valence из базовой линии), без вызовов модели и
// детерминированно от часов AGENT_TIMEZONE.
package character

import (
	"testing"
	"time"
)

// TestPersonaMoodTargetAt — цель затухания: valence базовая, arousal и
// socialEnergy — из блока текущего времени; без расписания — прежняя базовая
// линия.
func TestPersonaMoodTargetAt(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	persona := Persona{
		MoodBaseline: Mood{Valence: 0.1, Arousal: 0.7, SocialEnergy: 0.9},
		Schedule:     &sched,
	}

	day := persona.MoodTargetAt(time.Date(2026, 11, 3, 12, 0, 0, 0, loc), loc)
	if day.Valence != 0.1 {
		t.Fatalf("valence уехала из базовой линии: %+v", day)
	}
	if day.Arousal != 0.65 || day.SocialEnergy != 0.8 {
		t.Fatalf("цель дня = %+v, хочу arousal 0.65/social 0.8", day)
	}

	night := persona.MoodTargetAt(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc)
	if night.Arousal != 0.15 || night.SocialEnergy != 0.1 {
		t.Fatalf("цель ночи = %+v, хочу arousal 0.15/social 0.1", night)
	}

	// Дрейф плавный: за 4 часа от дневного значения к ночному — ещё не ночь.
	drifted := DecayMood(day, night, 4*time.Hour, MoodDecayLambdaPerHour)
	if drifted.Arousal <= night.Arousal || drifted.Arousal >= day.Arousal {
		t.Fatalf("затухание не тянет к блоку: %+v", drifted)
	}

	bare := Persona{MoodBaseline: Mood{Valence: -0.2, Arousal: 0.4, SocialEnergy: 0.6}}
	if got := bare.MoodTargetAt(time.Date(2026, 11, 3, 3, 0, 0, 0, loc), loc); got != bare.MoodBaseline {
		t.Fatalf("без расписания цель = %+v, хочу базовую линию", got)
	}
}

// TestPersonaMoodTargetHourly — цель меняется от часа к часу по одному и тому
// же мгновению в разных поясах: часовой пояс — часть поведения, а не деталь.
func TestPersonaMoodTargetHourly(t *testing.T) {
	sched := parsedSchedule(t)
	loc := moscow(t)
	persona := Persona{MoodBaseline: Mood{Valence: 0.1, Arousal: 0.7, SocialEnergy: 0.9}, Schedule: &sched}
	instant := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) // 12:00 МСК — «на работе»

	if got := persona.MoodTargetAt(instant, loc); got.Arousal != 0.65 {
		t.Fatalf("в Москве цель дня = %+v", got)
	}
	if got := persona.MoodTargetAt(instant, time.UTC); got.Arousal != 0.35 {
		t.Fatalf("в UTC цель утра = %+v", got)
	}
}
