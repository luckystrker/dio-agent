//go:build db

// БД-тесты частичной уникальности dedupe_key намерений (§14.1, блок 17.5):
// уникальность действует только на живые намерения (pending/running).
// Терминальное (completed/cancelled/failed) больше не блокирует повторное —
// раньше reply_later с темой = последний текст пользователя навсегда терял
// повторную просьбу теми же словами.
package character

import (
	"context"
	"sync"
	"testing"
)

// createIntentionInTx — намерение через общий путь создания (§14.1).
func createIntentionInTx(t *testing.T, ctx context.Context, f *fixture, topic string) (int64, bool) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("транзакция не открыта: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, created, err := CreateIntentionTx(ctx, tx, IntentionParams{
		Kind: "reply_later", ChatID: testChatID, ScopeID: testScopeID,
		Topic: topic, Reason: "тест 17.5: " + topic, DelayHours: -1,
	})
	if err != nil {
		t.Fatalf("намерение %q не создано: %v", topic, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("транзакция не зафиксирована: %v", err)
	}
	return id, created
}

// TestIntentionDedupeOnlyLiveBlocks — живое намерение блокирует дубль;
// терминальное — нет: та же тема создаётся заново, и новый живой дубль снова
// блокируется.
func TestIntentionDedupeOnlyLiveBlocks(t *testing.T) {
	f, ctx := newFixture(t)
	const topic = "скинь потом тот трек"

	first, created := createIntentionInTx(t, ctx, f, topic)
	if !created {
		t.Fatal("первое намерение должно создаться")
	}

	// Живой дубль (pending) — не создаётся (§14.1: перефразировки не размножают).
	if _, created := createIntentionInTx(t, ctx, f, topic); created {
		t.Fatal("живое намерение обязано блокировать дубль (pending)")
	}

	// Терминальное (completed) — не блокирует: тема отработана, повторная
	// просьба теми же словами — новое намерение.
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'completed' WHERE id = $1`, first); err != nil {
		t.Fatalf("намерение не закрыто: %v", err)
	}
	second, created := createIntentionInTx(t, ctx, f, topic)
	if !created {
		t.Fatal("терминальное намерение не должно блокировать повторное (completed)")
	}

	// Новый живой дубль снова блокируется.
	if _, created := createIntentionInTx(t, ctx, f, topic); created {
		t.Fatal("после создания нового намерения его живой дубль обязан блокироваться")
	}
	_ = second
}

// TestIntentionDedupeRunningAlsoBlocks — running тоже живой: исполняемое
// намерение блокирует дубль, как и pending.
func TestIntentionDedupeRunningAlsoBlocks(t *testing.T) {
	f, ctx := newFixture(t)
	const topic = "вернусь к этому вечером"

	first, _ := createIntentionInTx(t, ctx, f, topic)
	if _, err := f.pool.Exec(ctx,
		`UPDATE intentions SET status = 'running' WHERE id = $1`, first); err != nil {
		t.Fatalf("намерение не переведено в running: %v", err)
	}
	if _, created := createIntentionInTx(t, ctx, f, topic); created {
		t.Fatal("running-намерение обязано блокировать дубль")
	}
}

// TestIntentionDedupeTerminalCancelledFailedAlsoFree — остальные терминальные
// статусы (cancelled, failed) ведут себя как completed: отработанное/умершее
// намерение не блокирует повторную просьбу теми же словами (блок 17.5).
func TestIntentionDedupeTerminalCancelledFailedAlsoFree(t *testing.T) {
	for _, status := range []string{"cancelled", "failed"} {
		f, ctx := newFixture(t)
		const topic = "скинь потом тот трек"

		first, _ := createIntentionInTx(t, ctx, f, topic)
		if _, err := f.pool.Exec(ctx,
			`UPDATE intentions SET status = $1 WHERE id = $2`, status, first); err != nil {
			t.Fatalf("намерение не переведено в %s: %v", status, err)
		}
		if _, created := createIntentionInTx(t, ctx, f, topic); !created {
			t.Fatalf("терминальное намерение (%s) не должно блокировать повторное", status)
		}
	}
}

// TestIntentionDedupeConcurrentLiveDuplicate — два одновременных создания
// одного dedupe-ключа (гонка прямого хода и восстановления): ровно одно
// намерение создаётся, проигравший получает ветку created=false без ошибки
// уникальности (§14.1, блок 17.5 — частичный индекс под конкурентной вставкой).
func TestIntentionDedupeConcurrentLiveDuplicate(t *testing.T) {
	f, ctx := newFixture(t)
	const topic = "расскажи потом про выставку"

	type outcome struct {
		created bool
		err     error
	}
	start := make(chan struct{})
	outcomes := make([]outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			_, created, err := CreateIntentionTx(ctx, tx, IntentionParams{
				Kind: "reply_later", ChatID: testChatID, ScopeID: testScopeID,
				Topic: topic, Reason: "тест 17.5: конкурентный дубль", DelayHours: -1,
			})
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			if err := tx.Commit(ctx); err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			outcomes[i] = outcome{created: created}
		}(i)
	}
	close(start)
	wg.Wait()

	createdN := 0
	for i, o := range outcomes {
		if o.err != nil {
			t.Fatalf("конкурентное создание %d упало: %v — живой дубль обязан разрешаться веткой created=false, а не ошибкой (§14.1)", i, o.err)
		}
		if o.created {
			createdN++
		}
	}
	if createdN != 1 {
		t.Fatalf("создано намерений %d, хочу ровно 1 — конкурентный живой дубль прошёл дважды", createdN)
	}
	var rows int64
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM intentions WHERE topic = $1`, topic).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("строк намерений с темой %d (err=%v), хочу 1 — уникальность живого dedupe_key нарушена", rows, err)
	}
}
