//go:build db

// БД-тесты этапа 41 (исправления по независимому ревью этапов 33–39):
// M1 — восстановление качает только проверенный при генерации адрес картинки
// (и сохраняет alt, m18); M3 — прямой вопрос с пустым answer после repair не
// превращается в молчаливый ignore; M6 — defer при исчерпанном дневном лимите
// инициатив всё равно создаёт reply_later.
package character

import (
	"testing"

	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/web"
)

// TestRecoveryDoesNotFetchUnverifiedImageDB — M1: model_output без вердикта
// (запись старой версии или до проверки) не даёт восстановлению скачать
// выдуманный моделью URL.
func TestRecoveryDoesNotFetchUnverifiedImageDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 4101, "скинь картинку")
	if err := f.journal.WriteModelOutput(ctx, 4101, TurnResult{
		Decision: DecisionAnswer, Messages: []string{"вот"},
		ImageURL: "https://evil.example/x.png",
	}, nil, 0, "", "", nil); err != nil {
		t.Fatalf("model_output: %v", err)
	}
	fetcher := &fakeImageFetcher{img: &web.Image{
		URL: "https://evil.example/x.png", ContentType: "image/jpeg", Bytes: []byte("x"),
	}}
	f.runner.ImageFetcher = fetcher

	out, err := f.runner.Run(ctx, Input{TurnID: 4101, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "скинь картинку"}}})
	if err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	if !out.Recovered || !out.Apply.Applied {
		t.Fatalf("сохранённый результат не применён: %+v", out)
	}
	if len(fetcher.got) != 0 {
		t.Fatalf("восстановление скачало URL без вердикта проверки: %v", fetcher.got)
	}
	var photos int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'photo'`, 4101).
		Scan(&photos); err != nil {
		t.Fatalf("outbox не прочитан: %v", err)
	}
	if photos != 0 {
		t.Fatalf("непроверенная картинка попала в outbox: %d", photos)
	}
}

// TestRecoveryUsesSavedImageVerdictDB — M1/m18: сохранённый вердикт даёт
// картинку при восстановлении, alt не теряется.
func TestRecoveryUsesSavedImageVerdictDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 4102, "скинь кота")
	if err := f.journal.WriteModelOutputWithImage(ctx, 4102, TurnResult{
		Decision: DecisionAnswer, Messages: []string{"вот"},
		ImageURL: "https://cdn.example.com/a.jpg",
	}, nil, 0, "", "", nil, imageVerdict{URL: "https://cdn.example.com/a.jpg", Alt: "кот"}); err != nil {
		t.Fatalf("model_output: %v", err)
	}
	fetcher := &fakeImageFetcher{img: &web.Image{
		URL: "https://cdn.example.com/a.jpg", ContentType: "image/jpeg", Bytes: []byte("jpeg"),
	}}
	f.runner.ImageFetcher = fetcher

	out, err := f.runner.Run(ctx, Input{TurnID: 4102, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "скинь кота"}}})
	if err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	if !out.Apply.Applied {
		t.Fatalf("ход не применён: %+v", out)
	}
	if len(fetcher.got) != 1 || fetcher.got[0] != "https://cdn.example.com/a.jpg" {
		t.Fatalf("восстановление скачало %v, хочу сохранённый вердикт", fetcher.got)
	}
	var alt string
	if err := f.pool.QueryRow(ctx, `
		SELECT o.payload->>'alt' FROM delivery_outbox o
		WHERE o.turn_id = $1 AND o.payload->>'kind' = 'photo'`, 4102).Scan(&alt); err != nil {
		t.Fatalf("пузырь картинки не создан: %v", err)
	}
	if alt != "кот" {
		t.Fatalf("alt картинки восстановления = %q, хочу «кот» (m18)", alt)
	}
}

// TestRunDirectEmptyAnswerIsSkippedDB — M3: на прямом ходе пустой answer после
// repair не деградирует в ignore (вопрос съеден молча): терминальный пропуск с
// причиной в журнале.
func TestRunDirectEmptyAnswerIsSkippedDB(t *testing.T) {
	f, ctx := newFixture(t)
	f.seedTurn(t, ctx, 4103, "во сколько встречаемся?")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":[]}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":[]}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: 4103, ChatID: testChatID, ScopeID: testScopeID, Direct: true,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "во сколько встречаемся?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if !out.Skipped {
		t.Fatalf("прямой ход с пустым answer не пропущен терминально: %+v", out)
	}
	if out.Apply.Applied {
		t.Fatalf("пустой answer молча применён как ignore: %+v", out.Apply)
	}
	if rows := f.outboxRows(t, ctx, 4103); len(rows) != 0 {
		t.Fatalf("прямой ход с пустым answer что-то отправил: %v", rows)
	}
}

// TestApplyDefersReplyLaterAtDailyLimitDB — M6: исчерпанный дневной лимит
// инициатив не съедает отложенный ответ на прямой вопрос.
func TestApplyDefersReplyLaterAtDailyLimitDB(t *testing.T) {
	f, ctx := newFixture(t)
	seedExhaustedInitiativeLimit(t, ctx, f)
	f.seedTurn(t, ctx, 4104, "ответишь позже?")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"defer",` +
		`"defer_hours":2,"reason":"сейчас не до разговора"}`}})

	out, err := f.runner.Run(ctx, Input{TurnID: 4104, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ответишь позже?"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if len(out.Apply.IntentionIDs) != 1 {
		t.Fatalf("defer при исчерпанном лимите не создал reply_later: %+v", out.Apply)
	}
	if len(out.Apply.LimitedIntentions) != 0 {
		t.Fatalf("defer отмечен как ограниченный лимитом: %+v", out.Apply.LimitedIntentions)
	}
	var kind string
	if err := f.pool.QueryRow(ctx, `SELECT kind FROM intentions WHERE id = $1`,
		out.Apply.IntentionIDs[0]).Scan(&kind); err != nil {
		t.Fatalf("намерение не найдено: %v", err)
	}
	if kind != "reply_later" {
		t.Fatalf("создано намерение %q, хочу reply_later", kind)
	}
}
