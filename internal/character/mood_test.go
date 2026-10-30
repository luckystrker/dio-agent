// Юнит-тесты mood §7.2: затухание к baseline, ограничение дельт,
// детерминированное текстовое представление. Поведение — src/character/mood.ts
// (включая PBT-инварианты затухания этапа 4).
package character

import (
	"math"
	"testing"
	"time"
)

func TestDecayMoodReachesBaseline(t *testing.T) {
	stored := Mood{Valence: 0.9, Arousal: 0.9, SocialEnergy: 0.9}
	baseline := Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6}

	// Полураспад ≈ 1.4 ч: через 1.4 ч отклонение примерно вдвое меньше.
	got := DecayMood(stored, baseline, 84*time.Minute, MoodDecayLambdaPerHour)
	if d := got.Valence - baseline.Valence; math.Abs(d-0.4) > 0.01 {
		t.Fatalf("через полураспад отклонение valence = %v, хочу ≈0.4", d)
	}
	// Большое время → почти baseline.
	got = DecayMood(stored, baseline, 24*time.Hour, MoodDecayLambdaPerHour)
	if math.Abs(got.Valence-baseline.Valence) > 0.001 ||
		math.Abs(got.Arousal-baseline.Arousal) > 0.001 ||
		math.Abs(got.SocialEnergy-baseline.SocialEnergy) > 0.001 {
		t.Fatalf("за сутки mood не затух к baseline: %+v", got)
	}
	// Нулевое время → stored без изменений.
	got = DecayMood(stored, baseline, 0, MoodDecayLambdaPerHour)
	if got != stored {
		t.Fatalf("нулевое время изменило mood: %+v", got)
	}
}

func TestDecayMoodDeterministic(t *testing.T) {
	// Детерминизм (§7.2): одинаковый вход — одинаковый выход.
	baseline := Mood{Valence: 0, Arousal: 0.5, SocialEnergy: 0.5}
	stored := Mood{Valence: -0.7, Arousal: 0.2, SocialEnergy: 0.8}
	a := DecayMood(stored, baseline, 2*time.Hour, MoodDecayLambdaPerHour)
	b := DecayMood(stored, baseline, 2*time.Hour, MoodDecayLambdaPerHour)
	if a != b {
		t.Fatalf("затухание недетерминировано: %+v против %+v", a, b)
	}
}

func TestClampMoodDeltaLimits(t *testing.T) {
	v := 1.0
	a := 0.9
	s := -0.9
	clamped := ClampMoodDelta(MoodDelta{Valence: &v, Arousal: &a, SocialEnergy: &s, Reason: "тест"})
	if *clamped.Valence != MaxMoodDeltaPerTurn {
		t.Fatalf("valence не сжат до лимита: %v", *clamped.Valence)
	}
	if *clamped.Arousal != MaxMoodDeltaPerTurn || *clamped.SocialEnergy != -MaxMoodDeltaPerTurn {
		t.Fatalf("arousal/socialEnergy не сжаты: %+v", clamped)
	}
	if clamped.Reason != "тест" {
		t.Fatalf("основание потеряно: %q", clamped.Reason)
	}
}

func TestClampMoodDeltaKeepsNil(t *testing.T) {
	clamped := ClampMoodDelta(MoodDelta{Reason: "r"})
	if clamped.Valence != nil || clamped.Arousal != nil || clamped.SocialEnergy != nil {
		t.Fatalf("не заданные параметры получили значения: %+v", clamped)
	}
}

func TestDescribeMoodDeterministicBands(t *testing.T) {
	cases := []struct {
		mood Mood
		want string
	}{
		{Mood{Valence: 0.5, Arousal: 0.7, SocialEnergy: 0.8}, "в приподнятом настроении, взволнован, полон желания общаться"},
		{Mood{Valence: -0.5, Arousal: 0.2, SocialEnergy: 0.2}, "в подавленном настроении, спокоен, мало сил на общение"},
		{Mood{Valence: 0, Arousal: 0.5, SocialEnergy: 0.5}, "в ровном настроении"},
	}
	for _, tc := range cases {
		if got := DescribeMood(tc.mood); got != tc.want {
			t.Fatalf("DescribeMood(%+v) = %q, хочу %q", tc.mood, got, tc.want)
		}
	}
}
