//go:build db

// БД-тесты шкалы скуки (этап 23.1): чтение с ростом и записью, тихие блоки,
// сброс разговорным ходом через применение и спад от находки. Проверяется
// проводка в character_state (миграция 000016), а не только чистая формула.
package character

import (
	"context"
	"math"
	"testing"
	"time"
)

func seedBoredom(t *testing.T, ctx context.Context, f *fixture, value float64, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO character_state (id, boredom, boredom_updated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE
		  SET boredom = EXCLUDED.boredom, boredom_updated_at = EXCLUDED.boredom_updated_at`,
		value, at); err != nil {
		t.Fatalf("скука не засеяна: %v", err)
	}
}

func readBoredom(t *testing.T, ctx context.Context, f *fixture) (float64, time.Time) {
	t.Helper()
	var (
		value float64
		at    time.Time
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT boredom, boredom_updated_at FROM character_state WHERE id = 1`).Scan(&value, &at); err != nil {
		t.Fatalf("скука не прочитана: %v", err)
	}
	return value, at
}

// TestBoredomReadGrowsAndPersistsDB — время прошло, скука доросла и записалась
// с новой отметкой; в тихом блоке не растёт.
func TestBoredomReadGrowsAndPersistsDB(t *testing.T) {
	f, ctx := newFixture(t)
	now := time.Now().UTC()
	seedBoredom(t, ctx, f, 0.4, now.Add(-2*time.Hour))

	got, err := NewBoredomRepo().ReadGrown(ctx, f.pool, now, nil, nil, f.persona.MoodBaseline)
	if err != nil {
		t.Fatalf("ReadGrown: %v", err)
	}
	want := 0.4 + 2*BoredomGrowthPerHour
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("скука за 2 часа = %v, хочу %v", got, want)
	}
	stored, at := readBoredom(t, ctx, f)
	if math.Abs(stored-want) > 1e-6 {
		t.Fatalf("в хранилище %v, хочу %v", stored, want)
	}
	if at.Sub(now) < -time.Second || at.Sub(now) > time.Second {
		t.Fatalf("отметка роста = %v, хочу ~%v", at, now)
	}

	// Ночь (тихий блок 01:00–08:00): скука стоит на месте.
	loc := time.UTC
	schedule := Schedule{
		{StartMinute: 60, EndMinute: 8 * 60, Activity: "спит", Quiet: true},
		{StartMinute: 8 * 60, EndMinute: 23 * 60, Activity: "дела"},
		{StartMinute: 23 * 60, EndMinute: 60, Activity: "перед сном", Quiet: true},
	}
	night := time.Date(2026, 11, 3, 23, 30, 0, 0, loc)
	seedBoredom(t, ctx, f, 0.4, night)
	if got, err := NewBoredomRepo().ReadGrown(ctx, f.pool, night.Add(2*time.Hour), &schedule, loc, f.persona.MoodBaseline); err != nil {
		t.Fatalf("ReadGrown ночью: %v", err)
	} else if got != 0.4 {
		t.Fatalf("в тихом блоке скука выросла до %v", got)
	}

	// Дневное бодрствование с расписанием растёт: 08:00 → 12:00 = 4 часа.
	dayFrom := time.Date(2026, 11, 4, 8, 0, 0, 0, loc)
	seedBoredom(t, ctx, f, 0, dayFrom)
	want = 4 * BoredomGrowthPerHour
	if got, err := NewBoredomRepo().ReadGrown(ctx, f.pool, dayFrom.Add(4*time.Hour), &schedule, loc, f.persona.MoodBaseline); err != nil {
		t.Fatalf("ReadGrown днём: %v", err)
	} else if math.Abs(got-want) > 1e-6 {
		t.Fatalf("скучка за 4 дневных часа = %v, хочу %v", got, want)
	}
}

// TestBoredomCreatesStateWithMoodBaselineDB — первая запись скуки на чистой БД
// не должна подменять настроение дефолтами таблицы: строка создаётся с текущей
// целью настроения (блок расписания/база персоны), как вернул бы ReadDecayed
// при отсутствии строки.
func TestBoredomCreatesStateWithMoodBaselineDB(t *testing.T) {
	f, ctx := newFixture(t)
	baseline := Mood{Valence: -0.3, Arousal: 0.7, SocialEnergy: 0.9}
	now := time.Now().UTC()

	if got, err := NewBoredomRepo().ReadGrown(ctx, f.pool, now, nil, nil, baseline); err != nil {
		t.Fatalf("ReadGrown на чистой БД: %v", err)
	} else if got != 0 {
		t.Fatalf("скука на чистой БД = %v, хочу 0", got)
	}

	var (
		valence, arousal, social float64
	)
	if err := f.pool.QueryRow(ctx,
		`SELECT valence, arousal, social_energy FROM character_state WHERE id = 1`).
		Scan(&valence, &arousal, &social); err != nil {
		t.Fatalf("строка состояния не создана: %v", err)
	}
	if math.Abs(valence-baseline.Valence) > 1e-6 ||
		math.Abs(arousal-baseline.Arousal) > 1e-6 ||
		math.Abs(social-baseline.SocialEnergy) > 1e-6 {
		t.Fatalf("настроение при создании строки = %v/%v/%v, хочу цель %+v",
			valence, arousal, social, baseline)
	}

	// ReadDecayed сразу после создания возвращает ту же цель, а не дефолты.
	mood, _, err := NewMoodRepo().ReadDecayed(ctx, f.pool, baseline)
	if err != nil {
		t.Fatalf("настроение после создания строки не читается: %v", err)
	}
	if math.Abs(mood.Valence-baseline.Valence) > 1e-6 ||
		math.Abs(mood.Arousal-baseline.Arousal) > 1e-6 ||
		math.Abs(mood.SocialEnergy-baseline.SocialEnergy) > 1e-6 {
		t.Fatalf("ReadDecayed вернул %+v, хочу %+v", mood, baseline)
	}
}

// TestBoredomEventsDB — разговор сбрасывает в ноль, находка explore — заметно вниз.
func TestBoredomEventsDB(t *testing.T) {
	f, ctx := newFixture(t)
	now := time.Now().UTC()

	seedBoredom(t, ctx, f, 0.8, now)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	next, err := NewBoredomRepo().ApplyEvent(ctx, tx, BoredomConversation, now, nil, nil, f.persona.MoodBaseline)
	if err != nil {
		t.Fatalf("событие разговора: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("транзакция не зафиксирована: %v", err)
	}
	if next != 0 {
		t.Fatalf("разговор сбросил скуку до %v, хочу 0", next)
	}
	if stored, _ := readBoredom(t, ctx, f); stored != 0 {
		t.Fatalf("в хранилище после разговора %v", stored)
	}

	seedBoredom(t, ctx, f, 0.9, now)
	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	next, err = NewBoredomRepo().ApplyEvent(ctx, tx, BoredomExplore, now, nil, nil, f.persona.MoodBaseline)
	if err != nil {
		t.Fatalf("событие находки: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("транзакция не зафиксирована: %v", err)
	}
	if math.Abs(next-0.4) > 1e-6 {
		t.Fatalf("находка сбросила скуку до %v, хочу 0.4", next)
	}
}

// TestBoredomConversationTurnResetsDB — проводка: применение разговорного хода
// с флагом BoredomReset сбрасывает шкалу в той же транзакции.
func TestBoredomConversationTurnResetsDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 301, "привет, как ты?")
	seedBoredom(t, ctx, f, 0.8, time.Now().UTC().Add(-3*time.Hour))

	res, err := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID:          301,
		ChatID:          testChatID,
		ScopeID:         testScopeID,
		Result:          TurnResult{Decision: DecisionIgnore},
		PersonaBaseline: f.persona.MoodBaseline,
		Journal:         f.journal,
		BoredomReset:    true,
	})
	if err != nil {
		t.Fatalf("применение хода: %v", err)
	}
	if !res.Applied {
		t.Fatal("ход не применён")
	}
	if stored, _ := readBoredom(t, ctx, f); stored != 0 {
		t.Fatalf("разговорный ход не сбросил скуку: %v", stored)
	}
}
