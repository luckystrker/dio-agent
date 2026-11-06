//go:build db

// БД-тест 40.3 для фонового пути: reasoning_content провайдера достаёт
// транспорт (ago v0.6.0 в не-стрим ответе его не читает), а runTurnModel
// отдаёт его в turnModelRun.Reasoning. Дубль middleware-теста, но по проводу.
package background

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/modebug"
)

func TestRunTurnModelReasoningFromTransportDB(t *testing.T) {
	f, ctx := newFixture(t)

	body, err := json.Marshal(`{"decision":"ignore"}`)
	if err != nil {
		t.Fatalf("ответ не сериализуется: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":` +
			string(body) + `,"reasoning_content":"думаю в фоне"}}]}`))
	}))
	t.Cleanup(server.Close)

	// В тесте приложение не запускается — обёртку транспорта ставим руками
	// (в бою это делает app.Start).
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	modebug.InstallDefaultTransport(nil)

	f.sched.model = character.NewChatConfig(server.URL, "test-key", "test-model", 1000, 0, 0)
	run, err := f.sched.runTurnModel(ctx, "model:bg:intention:42:1", "система", "как дела?",
		nil, bgScope, "", nil)
	if err != nil {
		t.Fatalf("фоновый модельный ход: %v", err)
	}
	if run.Reasoning != "думаю в фоне" {
		t.Fatalf("reasoning фона из транспорта = %q", run.Reasoning)
	}
	if run.ReasoningRepair != "" {
		t.Fatalf("reasoning repair при валидном ответе = %q", run.ReasoningRepair)
	}
}

// TestRunTurnModelGarbageRepairDB — 40.4 в фоне: валидный JSON с мусором в
// messages чинится одним repair'ом через тот же общий помощник, что и в
// разговорном ходе, а не доставляется пользователю как есть.
func TestRunTurnModelGarbageRepairDB(t *testing.T) {
	f, ctx := newFixture(t)

	replies := []string{
		`{"decision":"answer","messages":[",","reply_to_by_message:,"]}`,
		`{"decision":"answer","messages":["привет, как дела?"]}`,
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := calls
		calls++
		if idx >= len(replies) {
			idx = len(replies) - 1
		}
		body, _ := json.Marshal(replies[idx])
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":` +
			string(body) + `}}]}`))
	}))
	t.Cleanup(server.Close)

	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })
	modebug.InstallDefaultTransport(nil)

	f.sched.model = character.NewChatConfig(server.URL, "test-key", "test-model", 1000, 0, 0)
	run, err := f.sched.runTurnModel(ctx, "model:bg:intention:43:1", "система", "как дела?",
		nil, bgScope, "", nil)
	if err != nil {
		t.Fatalf("фоновый модельный ход: %v", err)
	}
	if calls != 2 {
		t.Fatalf("модельных вызовов %d, хочу 2 (брак + repair)", calls)
	}
	if len(run.Result.Messages) != 1 || run.Result.Messages[0] != "привет, как дела?" {
		t.Fatalf("фон доставил бы брак: %+v", run.Result)
	}
	if run.Repairs != 1 {
		t.Fatalf("repair фона = %d, хочу 1", run.Repairs)
	}
}
