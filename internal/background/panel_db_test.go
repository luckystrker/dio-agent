//go:build db

// БД-тест этапа 28.4: фоновая модельная задача видит панель состояния.
// Раньше настроение/отношение передавались в BuildSystemPrompt, а shipped-
// шаблон их игнорирует — инициатива, explore и события жизни шли с плоским
// контекстом. Теперь фон собирает компактную панель (время и «чем занят»,
// настроение, собеседник и отношение) и кладёт её ПЕРВОЙ частью
// user-сообщения, как разговорный ход.
package background

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/character"
)

// modelMessages — роли и тексты сообщений из тела запроса провайдеру.
type modelMessages struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (m modelMessages) text(i int) string {
	var s string
	if err := json.Unmarshal(m.Messages[i].Content, &s); err == nil {
		return s
	}
	// Части content: [{"type":"text","text":"..."}]
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Messages[i].Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// TestInitiativeUserMessageCarriesStatePanel — инициатива получает панель
// состояния первой частью user-сообщения: время с «чем занят» по расписанию,
// настроение, собеседник и отношение. System при этом статичен и не несёт
// динамики (кэш префикса не ломается).
func TestInitiativeUserMessageCarriesStatePanel(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	f.sched.location = loc
	now := time.Date(2026, 11, 3, 14, 30, 0, 0, loc)
	f.setNow(now)
	schedule := character.Schedule{{
		StartMinute: 0, EndMinute: 1440, Activity: "на работе, разгребаю задачи",
	}}
	f.sched.persona.Schedule = &schedule
	f.seedPrivateChat(t, ctx, "тихо в чате", now.Add(-2*time.Hour))
	f.createIntention(t, ctx, "check_in", "как дела", -1, bgChatID)

	var got modelMessages
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		resp := map[string]any{
			"id": "x", "model": "test",
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message": map[string]any{
					"content": `{"decision":"answer","messages":["привет, как ты там?"]}`,
				},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	f.sched.model = character.NewChatConfig(server.URL, "k", "test", 1000, 0, 0)

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт не удался: %v", err)
	}
	if len(got.Messages) < 2 {
		t.Fatalf("модельный запрос без сообщений: %+v", got)
	}

	// System — статика: динамики фона там нет (она в панели).
	system := got.text(0)
	if got.Messages[0].Role != "system" {
		t.Fatalf("первое сообщение не system: %+v", got.Messages[0])
	}
	for _, dynamic := range []string{"твоё состояние", "на работе", "отношение к собеседнику"} {
		if strings.Contains(system, dynamic) {
			t.Fatalf("динамика %q попала в system фона:\n%s", dynamic, system)
		}
	}

	// Последнее user-сообщение начинается с панели состояния.
	last := len(got.Messages) - 1
	userText := got.text(last)
	if got.Messages[last].Role != "user" {
		t.Fatalf("последнее сообщение не user: %+v", got.Messages[last])
	}
	if !strings.HasPrefix(userText, "[состояние хода — служебное, не от собеседника]") {
		t.Fatalf("user-сообщение фона не начинается с панели:\n%s", userText)
	}
	for _, want := range []string{
		"сейчас: вт 03.11 14:30",
		"на работе, разгребаю задачи",
		"ты говоришь с: Коля",
		"твоё состояние:",
		"отношение к собеседнику:",
	} {
		if !strings.Contains(userText, want) {
			t.Fatalf("в user-сообщении фона нет %q:\n%s", want, userText)
		}
	}
	if !strings.Contains(userText, "Твоё отложенное намерение") {
		t.Fatalf("задание инициативы потерялось:\n%s", userText)
	}
}

// TestLifeEventUserMessageCarriesStatePanel — событие жизни тоже получает
// панель состояния (время/«чем занят», настроение) первой частью сообщения;
// собеседника и отношения у внутренней задачи нет.
func TestLifeEventUserMessageCarriesStatePanel(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	f.sched.location = loc
	now := time.Date(2026, 11, 3, 9, 10, 0, 0, loc)
	f.setNow(now)
	schedule := character.Schedule{{
		StartMinute: 0, EndMinute: 1440, Activity: "собираюсь на работу",
	}}
	f.sched.persona.Schedule = &schedule

	userText, err := f.sched.lifeEventUserPrompt(ctx)
	if err != nil {
		t.Fatalf("сообщение события жизни не собрано: %v", err)
	}
	for _, want := range []string{
		"[состояние хода — служебное, не от собеседника]",
		"сейчас: вт 03.11 09:10",
		"собираюсь на работу",
		"твоё состояние:",
		"Придумай одно новое событие",
	} {
		if !strings.Contains(userText, want) {
			t.Fatalf("в сообщении события жизни нет %q:\n%s", want, userText)
		}
	}
	for _, unwanted := range []string{"ты говоришь с", "отношение к собеседнику"} {
		if strings.Contains(userText, unwanted) {
			t.Fatalf("событие жизни получило чатовую строку %q:\n%s", unwanted, userText)
		}
	}
}
