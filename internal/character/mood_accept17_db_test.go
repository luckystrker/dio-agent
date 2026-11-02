//go:build db

// Приёмка 17.11: недостающие сценарии параллельности mood. Тесты дева
// закрывают ApplyDelta-против-ReadDecayed и «свежую строку не трогаем»;
// здесь — две другие половины ТЗ: (1) два конкурентных ApplyDelta
// сериализуются FOR UPDATE и обе дельты выживают; (2) устаревшая строка
// с заметным затуханием реально фиксируется в хранилище (затухание честное,
// а не только «не пишем лишнего»).
package character

import (
	"testing"
	"time"
)

// TestAcceptMoodTwoConcurrentApplyDelta — ход 1 держит блокировку строки в
// незакоммиченной транзакции применения, ход 2 применяет свою дельту параллельно:
// SELECT ... FOR UPDATE обязан serialize применения, обе дельты ложатся одна
// на другую (0 + 0.2 + 0.1 = 0.3), ни одна не теряется.
func TestAcceptMoodTwoConcurrentApplyDelta(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewMoodRepo()
	baseline := Mood{Valence: 0, Arousal: 0.4, SocialEnergy: 0.6}

	// Свежая строка равна baseline — затухание в арифметике не участвует.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now())`,
		baseline.Valence, baseline.Arousal, baseline.SocialEnergy); err != nil {
		t.Fatalf("состояние не вставлено: %v", err)
	}

	// Ход 1: дельта применена, транзакция держит блокировку строки.
	tx1, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx1: %v", err)
	}
	defer tx1.Rollback(ctx)
	v1 := 0.2
	if _, err := repo.ApplyDelta(ctx, tx1, baseline, MoodDelta{Valence: &v1, Reason: "ход 1"}); err != nil {
		t.Fatalf("ApplyDelta(ход 1): %v", err)
	}

	// Ход 2 параллельно: его FOR UPDATE упирается в блокировку хода 1.
	tx2, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx2: %v", err)
	}
	defer tx2.Rollback(ctx)
	type res struct {
		m   Mood
		err error
	}
	done := make(chan res, 1)
	v2 := 0.1
	go func() {
		m, err := repo.ApplyDelta(ctx, tx2, baseline, MoodDelta{Valence: &v2, Reason: "ход 2"})
		done <- res{m, err}
	}()

	// Ждём, пока ход 2 гарантированно ждёт блокировку строки.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity не читается: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ApplyDelta хода 2 не дошёл до блокировки строки за 5 с")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("ApplyDelta(ход 2): %v", r.err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	// Обе дельты выжили: 0 + 0.2 + 0.1 = 0.3.
	var stored float64
	if err := f.pool.QueryRow(ctx, `SELECT valence FROM character_state WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if stored < 0.29 || stored > 0.31 {
		t.Fatalf("одна из дельт потеряна: valence = %v, хочу 0.3 (0 + 0.2 + 0.1)", stored)
	}
}

// TestAcceptMoodReadDecayedPersistsMeaningfulDecay — устаревшая строка с
// заметным затуханием фиксируется в хранилище: значения сдвинулись к baseline
// и updated_at обновился. «Не писать вслепую» не должно превращаться в
// «не писать никогда».
func TestAcceptMoodReadDecayedPersistsMeaningfulDecay(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewMoodRepo()
	baseline := Mood{Valence: 0, Arousal: 0.4, SocialEnergy: 0.6}

	old := Mood{Valence: 0.8, Arousal: 0.8, SocialEnergy: 0.8}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now() - interval '8 hours')`,
		old.Valence, old.Arousal, old.SocialEnergy); err != nil {
		t.Fatalf("состояние не вставлено: %v", err)
	}

	decayed, _, err := repo.ReadDecayed(ctx, f.pool, baseline)
	if err != nil {
		t.Fatalf("ReadDecayed: %v", err)
	}
	// За 8 ч (λ=0.5/ч) отклонение 0.8 отваливается до 0.8·e^-4 ≈ 0.015.
	if decayed.Valence > 0.05 {
		t.Fatalf("затухание не посчитано: valence = %v, хочу ≈0.015", decayed.Valence)
	}

	var storedValence float64
	var updatedAt time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT valence, updated_at FROM character_state WHERE id = 1`).
		Scan(&storedValence, &updatedAt); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if storedValence > 0.05 {
		t.Fatalf("затухнувшее значение не зафиксировано в хранилище: valence = %v, хочу ≈0.015", storedValence)
	}
	if time.Since(updatedAt) > time.Hour {
		t.Fatalf("updated_at не обновился при заметном затухании: %v", updatedAt)
	}
}
