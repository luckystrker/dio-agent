//go:build db

// Терминальный провал хода (29.1): после исчерпания попыток ход получает
// статус failed с причиной; recovery его не воскрешает (журнал —
// turn_failed, применение не переводит строку в applied), новая генерация
// не запускается.
package character

import (
	"testing"
)

// TestTurnFailedIsTerminalDB — ход с turn_failed в журнале читается как
// терминальный: Run не идёт к модели (даже без настроенной модели не
// возвращает ErrModelNotConfigured), а применение сохранённого результата
// не переводит failed-строку в applied.
func TestTurnFailedIsTerminalDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(1301)
	f.seedTurn(t, ctx, turnID, "вопрос, на котором ход сломался")

	const reason = "превышен лимит попыток (3): применение не удалось"
	if err := f.journal.WriteTurnFailed(ctx, turnID, "worker", reason); err != nil {
		t.Fatalf("turn_failed не записан: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE turns SET status = 'failed', failure_reason = $2 WHERE id = $1`, turnID, reason); err != nil {
		t.Fatalf("строка хода не переведена в failed: %v", err)
	}

	// Модель намеренно не настроена: терминальный ход не должен дойти до
	// генерации и вернуть ErrModelNotConfigured.
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "вопрос"}}})
	if err != nil {
		t.Fatalf("терминально проваленный ход снова пошёл в работу: %v", err)
	}
	if !out.Failed {
		t.Fatalf("исход хода не помечен провалом: %+v", out)
	}
	if out.FailReason != reason {
		t.Fatalf("причина провала %q, хочу %q", out.FailReason, reason)
	}

	// Применение сохранённого результата не воскрешает failed-ход: CAS
	// строки хода обязан отказать, иначе «отравленный» результат применится
	// поверх терминального провала.
	result, perr := ParseTurnResult([]byte(validFinalJSON()))
	if perr != nil {
		t.Fatalf("тестовый результат невалиден: %v", perr)
	}
	applied, aerr := ApplyTurn(ctx, f.pool, ApplyOptions{
		TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Result: result, Journal: f.journal,
	})
	if aerr != nil {
		t.Fatalf("применение проваленного хода упало: %v", aerr)
	}
	if applied.Applied {
		t.Fatal("применение перевело failed-ход в applied — terminal-статус не защищён")
	}
	var status string
	var storedReason *string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, failure_reason FROM turns WHERE id = $1`, turnID).Scan(&status, &storedReason); err != nil {
		t.Fatalf("ход не читается: %v", err)
	}
	if status != "failed" || storedReason == nil || *storedReason != reason {
		t.Fatalf("после применения status=%q reason=%v, хочу failed/%q", status, storedReason, reason)
	}
}
