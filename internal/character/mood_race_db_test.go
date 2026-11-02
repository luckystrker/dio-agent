//go:build db

// 17.11: конкурентные ApplyDelta и ReadDecayed. ReadDecayed зовёт каждая
// сборка контекста (включая горутину планировщика фона), ApplyDelta —
// транзакция применения хода; синглтон character_state один. До фикса
// ReadDecayed писал затухнувшее значение вслепую и мог затереть только что
// применённую дельту, а два ApplyDelta без блокировки строки теряли
// обновления друг друга (Runner заявляет параллельную безопасность).
package character

import (
	"testing"
	"time"
)

// TestMoodApplyDeltaNotLostAgainstReadDecayed — детерминированный
// интерливинг: чтение ReadDecayed состоялось до коммита дельты, запись
// затухания ждёт блокировку строки и выполняется после коммита. Дельта хода
// обязана выжить — затухание с устаревшего снимка не имеет права её затереть.
func TestMoodApplyDeltaNotLostAgainstReadDecayed(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewMoodRepo()
	baseline := Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6}

	// Устаревшее состояние: за сутки затухание почти полностью отвело значения
	// к baseline — запись ReadDecayed по этому снимку сильно отличается.
	old := Mood{Valence: 0.9, Arousal: 0.9, SocialEnergy: 0.9}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now() - interval '24 hours')`,
		old.Valence, old.Arousal, old.SocialEnergy); err != nil {
		t.Fatalf("состояние не вставлено: %v", err)
	}

	// Транзакция применения: ApplyDelta прочитал старое значение и записал
	// дельту, держит блокировку строки, коммита ещё нет.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	defer tx.Rollback(ctx)
	v := 0.25
	if _, err := repo.ApplyDelta(ctx, tx, baseline, MoodDelta{Valence: &v, Reason: "тест"}); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	// Параллельная сборка контекста: SELECT видит старый снимок (незакоммиченное
	// скрыто), UPDATE затухания упирается в блокировку строки.
	done := make(chan error, 1)
	go func() {
		_, _, err := repo.ReadDecayed(ctx, f.pool, baseline)
		done <- err
	}()

	// Ждём, пока ReadDecayed гарантированно дошёл до записи (ждёт блокировку),
	// — только тогда коммитим дельту.
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
			t.Fatal("ReadDecayed не дошёл до блокировки строки за 5 с")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ReadDecayed: %v", err)
	}

	// Дельта не затёрта: сутки затухания отвели базу к baseline (0.1),
	// дельта хода дала 0.35; слепая запись затухания со старого снимка дала бы
	// снова ≈0.1.
	var stored float64
	if err := f.pool.QueryRow(ctx, `SELECT valence FROM character_state WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if stored < 0.3 || stored > 0.4 {
		t.Fatalf("применённая дельта потеряна: valence = %v, хочу ≈0.35 (baseline+дельта)", stored)
	}
}

// TestMoodReadDecayedNoBlindWrite — свежая строка не переписывается: если
// затухание не изменило значения заметно, записи нет и updated_at не
// двигается (каждая сборка контекста больше не лезет в строку без причины).
func TestMoodReadDecayedNoBlindWrite(t *testing.T) {
	f, ctx := newFixture(t)
	repo := NewMoodRepo()
	baseline := Mood{Valence: 0.1, Arousal: 0.4, SocialEnergy: 0.6}

	m := Mood{Valence: 0.5, Arousal: 0.5, SocialEnergy: 0.5}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO character_state (id, valence, arousal, social_energy, updated_at)
		 VALUES (1, $1, $2, $3, now())`, m.Valence, m.Arousal, m.SocialEnergy); err != nil {
		t.Fatalf("состояние не вставлено: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // now() записи ReadDecayed был бы строго позже вставки

	if _, _, err := repo.ReadDecayed(ctx, f.pool, baseline); err != nil {
		t.Fatalf("ReadDecayed: %v", err)
	}
	var updatedAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT updated_at FROM character_state WHERE id = 1`).Scan(&updatedAt); err != nil {
		t.Fatalf("состояние не читается: %v", err)
	}
	if time.Since(updatedAt) < 10*time.Millisecond {
		t.Fatalf("ReadDecayed переписал свежую строку вслепую: updated_at = %v", updatedAt)
	}
}
