//go:build db

// Провод repair-продолжения на засеянном потоке (29.4, догонка приёмки 15):
// история чата и неудачный ответ модели остаются в потоке, repair-попытка
// получает и их, и repair-инструкцию — иначе модель повторяет тот же мусор,
// не видя, что именно не прошло валидацию.
package character

import (
	"strings"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestRepairContinuationCarriesHistoryAndHintDB — второй (repair) модельный
// вызов видит: реплику собеседника из истории, прежний ответ ассистента,
// текущую пачку, СВОЙ невалидный ответ и repair-инструкцию; ход применяет
// ответ repair-попытки.
func TestRepairContinuationCarriesHistoryAndHintDB(t *testing.T) {
	f, ctx := newFixture(t)

	// Прошлый ход: реплика и доставленный ответ — это история разговора.
	prev := int64(1401)
	f.seedTurn(t, ctx, prev, "обсуждали старые пластинки")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["ага, пластинки это святое"]}`,
	}})
	if _, err := f.runner.Run(ctx, Input{TurnID: prev, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "обсуждали старые пластинки"}}}); err != nil {
		t.Fatalf("прошлый ход не выполнен: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE delivery_outbox SET status='sent', telegram_message_id = 570001 WHERE turn_id = $1`, prev); err != nil {
		t.Fatalf("ответ прошлого хода не доставлен: %v", err)
	}

	// Текущий ход: первый ответ невалиден (answer без сообщений), repair — валиден.
	cur := int64(1402)
	f.seedTurn(t, ctx, cur, "а помнишь, что я люблю?")
	invalid := `{"decision":"answer","messages":[]}`
	cfg := f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: invalid}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["помню: пластинки"]}`}},
	)
	out, err := f.runner.Run(ctx, Input{TurnID: cur, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "а помнишь, что я люблю?"}}})
	if err != nil {
		t.Fatalf("ход с repair упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("repair не спас ход: %+v", out)
	}

	// Модель вызвана дважды: первая попытка и repair-продолжение.
	calls := cfg.Calls()
	if len(calls) != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (первый + repair)", len(calls))
	}
	wire := sp15WireOfCall(t, calls[1])
	if len(wire) != 5 {
		t.Fatalf("на проводе repair-вызова %d сообщений (%+v), хочу 5: история (2) + пачка + неудачный ответ + repair-инструкция", len(wire), wire)
	}
	if wire[0].role != "user" || !strings.HasSuffix(wire[0].text, "] Коля: обсуждали старые пластинки") {
		t.Fatalf("реплика истории потеряна: %+v", wire[0])
	}
	if wire[1].role != "assistant" || !strings.HasSuffix(wire[1].text, "] ага, пластинки это святое") {
		t.Fatalf("ответ истории потерян: %+v", wire[1])
	}
	if wire[2].role != "user" || !strings.HasSuffix(wire[2].text, "а помнишь, что я люблю?") {
		t.Fatalf("пачка текущего хода потеряна: %+v", wire[2])
	}
	if wire[3].role != "assistant" || wire[3].text != invalid {
		t.Fatalf("неудачный ответ модели не остался в потоке: %+v", wire[3])
	}
	if wire[4].role != "user" ||
		!strings.Contains(wire[4].text, "не прошёл валидацию") ||
		!strings.Contains(wire[4].text, "TurnResult") {
		t.Fatalf("repair-инструкция не доехала до модели: %+v", wire[4])
	}

	// Применён ответ repair-попытки.
	var text string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1`, cur).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "помню: пластинки" {
		t.Fatalf("применён не repair-ответ: %q", text)
	}
}
