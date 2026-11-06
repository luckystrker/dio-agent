// Тесты абляций replay (40.2): каждая абляция меняет только свою часть
// запроса, остальное остаётся как было.
package modebug

import (
	"encoding/json"
	"strings"
	"testing"
)

// replayBody — типичный разговорный запрос живого хода: system, история с
// метками/временем, панель состояния в последнем user, инструменты и схема.
func replayBody(t *testing.T) map[string]any {
	t.Helper()
	raw := `{
		"model": "test-model",
		"temperature": 0.7,
		"messages": [
			{"role":"system","content":"Ты — персонаж."},
			{"role":"user","content":"[пт 06.11 12:01] #m1034 Коля: привет"},
			{"role":"assistant","content":"[пт 06.11 12:02] привет"},
			{"role":"assistant","content":"[пт 06.11 12:02] как ты"},
			{"role":"user","content":"[пт 06.11 12:03] #m1035 Коля: норм"},
			{"role":"user","content":"[состояние хода — служебное, не от собеседника]\nсейчас: пт 06.11 12:04\n[/состояние]\n\n[пт 06.11 12:05] #m1036 Коля: а ты?"}
		],
		"tools": [{"type":"function","function":{"name":"memory_search"}}],
		"tool_choice": "none",
		"response_format": {"type":"json_schema","json_schema":{"name":"TurnResult","schema":{"type":"object"}}}
	}`
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("тестовое тело не JSON: %v", err)
	}
	return body
}

// messageTexts — тексты сообщений запроса по порядку.
func messageTexts(t *testing.T, body map[string]any) []string {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("в теле нет messages: %v", body)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm := m.(map[string]any)
		out = append(out, mm["content"].(string))
	}
	return out
}

func TestVariantNoLabels(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"no-labels"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	joined := strings.Join(messageTexts(t, body), "\n")
	if strings.Contains(joined, "#m") {
		t.Fatalf("метки #m остались: %q", joined)
	}
	if !strings.Contains(joined, "Коля: привет") {
		t.Fatalf("текст реплик потерян: %q", joined)
	}
}

func TestVariantNoTimestamps(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"no-timestamps"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	joined := strings.Join(messageTexts(t, body), "\n")
	if strings.Contains(joined, "12:01") || strings.Contains(joined, "[пт 06.11") {
		t.Fatalf("метки времени остались: %q", joined)
	}
	if !strings.Contains(joined, "Коля: привет") {
		t.Fatalf("текст реплик потерян: %q", joined)
	}
}

func TestVariantNoPanel(t *testing.T) {
	body := replayBody(t)
	markers := PanelMarkers{
		Open:  "[состояние хода — служебное, не от собеседника]",
		Close: "[/состояние]",
	}
	if _, err := ApplyVariants(body, []string{"no-panel"}, markers); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	texts := messageTexts(t, body)
	last := texts[len(texts)-1]
	if strings.Contains(last, markers.Open) || strings.Contains(last, markers.Close) {
		t.Fatalf("панель осталась в последнем сообщении: %q", last)
	}
	if !strings.Contains(last, "а ты?") {
		t.Fatalf("пачка потеряна при удалении панели: %q", last)
	}
	// Панель без маркеров — не трогаем (меньше удивления, ничего не ломаем).
	body2 := replayBody(t)
	if _, err := ApplyVariants(body2, []string{"no-panel"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция без маркеров: %v", err)
	}
}

func TestVariantHistoryLastN(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"history-last-2"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	msgs := body["messages"].([]any)
	// system + 2 последних сообщения.
	if len(msgs) != 3 {
		t.Fatalf("сообщений %d, хочу 3 (system + 2)", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("system потерян: %v", msgs[0])
	}
	texts := messageTexts(t, body)
	if !strings.Contains(texts[2], "а ты?") {
		t.Fatalf("последнее сообщение потеряно: %v", texts)
	}
	if _, err := ApplyVariants(replayBody(t), []string{"history-last"}, PanelMarkers{}); err == nil {
		t.Fatal("history-last без N должен быть ошибкой")
	}
}

func TestVariantAssistantAsJSON(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"assistant-as-json"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	texts := messageTexts(t, body)
	var wrapped int
	for _, text := range texts {
		if !strings.HasPrefix(text, `{"decision":"answer","messages":`) {
			continue
		}
		wrapped++
		var parsed struct {
			Decision string   `json:"decision"`
			Messages []string `json:"messages"`
		}
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			t.Fatalf("обёртка assistant не JSON: %v\n%s", err, text)
		}
		if parsed.Decision != "answer" || len(parsed.Messages) != 2 {
			t.Fatalf("обёрнуты не две подряд идущие реплики: %+v", parsed)
		}
	}
	if wrapped != 1 {
		t.Fatalf("обёрнутых групп %d, хочу 1: %v", wrapped, texts)
	}
}

func TestVariantMergeAssistant(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"merge-assistant"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	texts := messageTexts(t, body)
	if len(texts) != 5 {
		t.Fatalf("сообщений %d, хочу 5 (две assistant склеены): %v", len(texts), texts)
	}
	if !strings.Contains(texts[2], "привет") || !strings.Contains(texts[2], "как ты") {
		t.Fatalf("склейка assistant потеряла текст: %q", texts[2])
	}
}

func TestVariantNoToolsAndTemperature(t *testing.T) {
	body := replayBody(t)
	if _, err := ApplyVariants(body, []string{"no-tools", "temperature=0.2"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("tools остались: %v", body["tools"])
	}
	if _, ok := body["tool_choice"]; ok {
		t.Fatalf("tool_choice остался: %v", body["tool_choice"])
	}
	if got, _ := body["temperature"].(float64); got != 0.2 {
		t.Fatalf("temperature = %v, хочу 0.2", body["temperature"])
	}
	// Схема не меняется: абляции не трогают response_format.
	if _, ok := body["response_format"]; !ok {
		t.Fatal("response_format потерян")
	}
}

func TestVariantUnknown(t *testing.T) {
	if _, err := ApplyVariants(replayBody(t), []string{"nope"}, PanelMarkers{}); err == nil {
		t.Fatal("неизвестная абляция должна быть ошибкой")
	}
}
