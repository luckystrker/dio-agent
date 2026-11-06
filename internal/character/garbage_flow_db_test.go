//go:build db

// БД-тесты взаимодействия страховки 40.4 с обычным repair валидации (§11.3):
// второй repair не делается, брак чистится; неудачный repair брака оставляет
// первый (уже валидный по форме) ответ.
package character

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luckystrker/ago/events"
)

// TestGarbageAfterValidationRepairNoSecondRepairDB — первый ответ не прошёл
// валидацию, repair дал валидный формат, но брак в messages. Второй repair
// (40.4) не положен: repair уже был, брак выкидывается, все брак — деградация.
func TestGarbageAfterValidationRepairNoSecondRepairDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3011)
	f.seedTurn(t, ctx, turnID, "Ау?")
	cfg := f.withModel(
		// Валидация: answer без messages.
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer"}`,
		}},
		// Repair валидации: формат верный, но реплика — мусор.
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":[","]}`,
		}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Ау?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("страховка должна деградировать, а не пропускать: %+v", out)
	}
	if calls := len(cfg.Calls()); calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2: второй repair 40.4 при repairs==1 не положен", calls)
	}
	mo := f.modelOutputOf(t, ctx, turnID)
	if mo == nil {
		t.Fatal("model_output не записан")
	}
	if mo.Repairs != 1 {
		t.Fatalf("repairs=%d, хочу 1 (один repair валидации)", mo.Repairs)
	}
	var res TurnResult
	if err := json.Unmarshal(mo.Result, &res); err != nil {
		t.Fatalf("model_output.result не JSON: %v", err)
	}
	if res.Decision != DecisionIgnore || len(res.Messages) != 0 {
		t.Fatalf("после чистки ожидал ignore без сообщений: %+v", res)
	}
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&count); err != nil {
		t.Fatalf("счётчик outbox: %v", err)
	}
	if count != 0 {
		t.Fatalf("брак доставлен: %d сообщений", count)
	}
}

// TestGarbageRepairInvalidJSONKeepsFirstAnswerDB — repair брака не дал
// валидного TurnResult: остаётся первый ответ (форма валидна), из него
// выкидывается брак, нормальная реплика доставляется.
func TestGarbageRepairInvalidJSONKeepsFirstAnswerDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3012)
	f.seedTurn(t, ctx, turnID, "Ты тут?")
	cfg := f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":"answer","messages":[",","я тут"]}`,
		}},
		// Repair брака: обрывок JSON, не TurnResult.
		&events.ModelResponse{Message: &events.ModelMessage{
			Content: `{"decision":`,
		}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Ты тут?"}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применён: %+v", out)
	}
	if calls := len(cfg.Calls()); calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2", calls)
	}
	var text string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "я тут" {
		t.Fatalf("доставлено %q, хочу нормальную реплику первого ответа", text)
	}
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&count); err != nil {
		t.Fatalf("счётчик outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("сообщений в outbox %d, хочу 1 (брак выкинут)", count)
	}
}

// TestGarbageRepairAfterMediaFallbackDB — режим mediaFallback (25.1) и 40.4
// вместе: провайдер не принял картинку, повтор без картинок дал валидный
// формат с браком в messages, repair вернул живую реплику. Картинок в запросе
// больше нет, image_notes вычищены (37.1), брак не доставлен.
func TestGarbageRepairAfterMediaFallbackDB(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(3013)
	f.seedTurn(t, ctx, turnID, "смотри")

	var calls atomic.Int64
	var sawImage [3]bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n := int(calls.Add(1))
		hasImage := strings.Contains(string(raw), `"type":"image_url"`)
		if n <= len(sawImage) {
			sawImage[n-1] = hasImage
		}
		if hasImage {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid image format image/webp"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 2 {
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"decision\":\"answer\",\"messages\":[\",\"]}"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"{\"decision\":\"answer\",\"messages\":[\"вижу тебя\"],\"image_notes\":[\"закат\"]}"}}]}`)
	}))
	t.Cleanup(server.Close)

	f.runner.Photos = &recordingPhotoFetcher{record: func(string) {}}
	f.runner.Model = NewChatConfig(server.URL, "test-key", "test-model", 1000, 0, 0)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "смотри",
			Raw: json.RawMessage(`{"message":{"message_id":7001,"text":"смотри","photo":[{"file_id":"p1","width":10,"height":10}],"from":{"id":777}}}`)}}})
	if err != nil {
		t.Fatalf("ход: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("ход не применён: %+v", out)
	}

	var text string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'message'`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "вижу тебя" {
		t.Fatalf("доставлено %q, хочу repair-реплику", text)
	}
	mo := f.modelOutputOf(t, ctx, turnID)
	if mo == nil {
		t.Fatal("model_output не записан")
	}
	if mo.Repairs != 1 {
		t.Fatalf("repairs=%d, хочу 1 (только repair брака: откат по картинке — не repair)", mo.Repairs)
	}
	var res TurnResult
	if err := json.Unmarshal(mo.Result, &res); err != nil {
		t.Fatalf("model_output.result не JSON: %v", err)
	}
	if len(res.ImageNotes) != 0 {
		t.Fatalf("после mediaFallback остались image_notes: %v", res.ImageNotes)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("модельных HTTP-вызовов %d, хочу 3 (картинка → текст → repair)", got)
	}
	if !sawImage[0] || sawImage[1] || sawImage[2] {
		t.Fatalf("картинка в запросах: %v, хочу только в первом", sawImage)
	}
}
