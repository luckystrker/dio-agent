// Mood (§7.2): три числа с детерминированным затуханием к базовой линии
// persona. Фоновый LLM для decay не нужен. Изменения ограничены на ход и
// требуют основания; конкретная обида/радость живёт в памяти и отношениях,
// а не в числе. Поведение перенесено из TS-справочника src/character/mood.ts.
package character

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/luckystrker/dio-agent/internal/memory"
)

// DBTX — интерфейс доступа к БД (пул или транзакция pgx); общий с памятью.
type DBTX = memory.DBTX

// Mood — компактное эмоциональное состояние (§7.2).
type Mood struct {
	Valence      float64 `json:"valence"`      // -1..1
	Arousal      float64 `json:"arousal"`      // 0..1
	SocialEnergy float64 `json:"socialEnergy"` // 0..1
}

const (
	// MoodDecayLambdaPerHour — период полураспада отклонения ≈ 1.4 часа.
	MoodDecayLambdaPerHour = 0.5
	// MaxMoodDeltaPerTurn — максимальное изменение параметра за один ход (§7.2).
	MaxMoodDeltaPerTurn = 0.3
)

func clamp(v, min, max float64) float64 { return math.Min(max, math.Max(min, v)) }

// DecayMood — затухание к базовой линии: current = baseline + (stored-baseline)·e^(−λt).
func DecayMood(stored, baseline Mood, elapsed time.Duration, lambdaPerHour float64) Mood {
	factor := math.Exp(-lambdaPerHour * elapsed.Hours())
	return Mood{
		Valence:      clamp(baseline.Valence+(stored.Valence-baseline.Valence)*factor, -1, 1),
		Arousal:      clamp(baseline.Arousal+(stored.Arousal-baseline.Arousal)*factor, 0, 1),
		SocialEnergy: clamp(baseline.SocialEnergy+(stored.SocialEnergy-baseline.SocialEnergy)*factor, 0, 1),
	}
}

// ClampMoodDelta сжимает предложенную моделью дельту до ±MaxMoodDeltaPerTurn
// по каждому параметру. nil-параметр остаётся nil (не задан).
func ClampMoodDelta(delta MoodDelta) MoodDelta {
	out := MoodDelta{Reason: delta.Reason}
	if delta.Valence != nil {
		v := clamp(*delta.Valence, -MaxMoodDeltaPerTurn, MaxMoodDeltaPerTurn)
		out.Valence = &v
	}
	if delta.Arousal != nil {
		v := clamp(*delta.Arousal, -MaxMoodDeltaPerTurn, MaxMoodDeltaPerTurn)
		out.Arousal = &v
	}
	if delta.SocialEnergy != nil {
		v := clamp(*delta.SocialEnergy, -MaxMoodDeltaPerTurn, MaxMoodDeltaPerTurn)
		out.SocialEnergy = &v
	}
	return out
}

// DescribeMood — детерминированное текстовое представление (§7.2) для контекста
// хода. Сравнивается с базовой линией persona, а не с нулём: у Дио baseline
// valence 0.1, и абсолютные пороги прятали небольшие, но заметные сдвиги
// (портировано из TS-линии 132807e, 2026-10-31).
func DescribeMood(mood, baseline Mood) string {
	var parts []string
	v := mood.Valence - baseline.Valence
	switch {
	case v > 0.4:
		parts = append(parts, "в приподнятом настроении")
	case v > 0.12:
		parts = append(parts, "чуть оживлён")
	case v < -0.4:
		parts = append(parts, "в подавленном настроении")
	case v < -0.12:
		parts = append(parts, "не в духе")
	default:
		parts = append(parts, "в ровном настроении")
	}
	a := mood.Arousal - baseline.Arousal
	switch {
	case a > 0.2:
		parts = append(parts, "взволнован")
	case a < -0.15:
		parts = append(parts, "спокоен")
	}
	se := mood.SocialEnergy - baseline.SocialEnergy
	switch {
	case se < -0.2:
		parts = append(parts, "мало сил на общение")
	case se > 0.15:
		parts = append(parts, "полон желания общаться")
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

// round3 округляет до 3 знаков: артефакты плавающей точки не попадают в хранилище.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// MoodRepo — физическое настроение живёт в character_state (singleton id=1,
// миграция 000005); затухание к baseline вычисляется при чтении (§7.2).
type MoodRepo struct{}

// NewMoodRepo создаёт репозиторий (без состояния — методы принимают DBTX).
func NewMoodRepo() *MoodRepo { return &MoodRepo{} }

// ReadDecayed читает настроение с затуханием от времени последнего обновления
// и записывает затухнувшее значение обратно (следующая дельта ложится на свежую
// базу). Отсутствие строки = базовая линия persona.
func (r *MoodRepo) ReadDecayed(ctx context.Context, db DBTX, baseline Mood) (Mood, time.Time, error) {
	var (
		valence, arousal, social float64
		updatedAt                time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT valence, arousal, social_energy, updated_at FROM character_state WHERE id = 1`,
	).Scan(&valence, &arousal, &social, &updatedAt)
	if isNoRows(err) {
		return baseline, time.Now(), nil
	}
	if err != nil {
		return Mood{}, time.Time{}, fmt.Errorf("mood: чтение не удалось: %w", err)
	}
	stored := Mood{Valence: valence, Arousal: arousal, SocialEnergy: social}
	decayed := DecayMood(stored, baseline, time.Since(updatedAt), MoodDecayLambdaPerHour)
	_, err = db.Exec(ctx,
		`UPDATE character_state
		 SET valence = $1, arousal = $2, social_energy = $3, updated_at = now()
		 WHERE id = 1`, decayed.Valence, decayed.Arousal, decayed.SocialEnergy)
	if err != nil {
		return Mood{}, time.Time{}, fmt.Errorf("mood: затухнувшее значение не записано: %w", err)
	}
	return decayed, time.Now(), nil
}

// ApplyDelta применяет дельту хода ВНУТРИ транзакции применения (после сжатия):
// хранимое значение затухает к baseline, дельта ложится на свежую базу.
func (r *MoodRepo) ApplyDelta(ctx context.Context, db DBTX, baseline Mood, delta MoodDelta) (Mood, error) {
	var (
		valence, arousal, social float64
		updatedAt                time.Time
		has                      bool
	)
	err := db.QueryRow(ctx,
		`SELECT valence, arousal, social_energy, updated_at FROM character_state WHERE id = 1`,
	).Scan(&valence, &arousal, &social, &updatedAt)
	switch {
	case isNoRows(err):
		valence, arousal, social, updatedAt, has = baseline.Valence, baseline.Arousal, baseline.SocialEnergy, time.Now(), false
	case err != nil:
		return Mood{}, fmt.Errorf("mood: чтение перед дельтой не удалось: %w", err)
	default:
		has = true
	}

	var base Mood
	if has {
		base = DecayMood(
			Mood{Valence: valence, Arousal: arousal, SocialEnergy: social},
			baseline, time.Since(updatedAt), MoodDecayLambdaPerHour)
	} else {
		base = baseline
	}

	next := Mood{
		Valence:      round3(clamp(base.Valence+pointerOrZero(delta.Valence), -1, 1)),
		Arousal:      round3(clamp(base.Arousal+pointerOrZero(delta.Arousal), 0, 1)),
		SocialEnergy: round3(clamp(base.SocialEnergy+pointerOrZero(delta.SocialEnergy), 0, 1)),
	}
	_, err = db.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now())
		 ON CONFLICT (id) DO UPDATE
		   SET valence = EXCLUDED.valence, arousal = EXCLUDED.arousal,
		       social_energy = EXCLUDED.social_energy, updated_at = now()`,
		next.Valence, next.Arousal, next.SocialEnergy)
	if err != nil {
		return Mood{}, fmt.Errorf("mood: дельта не применена: %w", err)
	}
	return next, nil
}

func pointerOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
