// Дополнительные тесты абляций (40.2): нестандартный content (массив частей
// мультимодального сообщения), несколько system-сообщений, assistant с
// tool_calls и устойчивость к отсутствующим полям.
package modebug

import (
	"encoding/json"
	"strings"
	"testing"
)

// partsBody — запрос с мультимодальным content: текстовая часть + картинка.
// В последнем user — панель состояния внутри текстовой части.
func partsBody() map[string]any {
	return map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "Ты — персонаж."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "[пт 06.11 12:01] #m1 Коля: привет"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/webp;base64,AAA"}},
			}},
			map[string]any{"role": "assistant", "content": "[пт 06.11 12:02] привет"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "[состояние хода]\nсейчас: пт 06.11 12:04\n[/состояние]\n\n#m2 Коля: а ты?"},
			}},
		},
	}
}

// textParts — текстовые части сообщения (для проверок мультимодального content).
func textParts(t *testing.T, content any) []string {
	t.Helper()
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("content не массив частей: %T", content)
	}
	var out []string
	for _, part := range parts {
		pm := part.(map[string]any)
		if pm["type"] == "text" {
			out = append(out, pm["text"].(string))
		}
	}
	return out
}

// TestVariantLabelsAndTimestampsInContentParts — no-labels/no-timestamps
// работают с массивом частей (мультимодальный ход), картинку не трогают.
func TestVariantLabelsAndTimestampsInContentParts(t *testing.T) {
	body := partsBody()
	if _, err := ApplyVariants(body, []string{"no-labels", "no-timestamps"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляции: %v", err)
	}
	msgs := body["messages"].([]any)
	first := textParts(t, msgs[1].(map[string]any)["content"])
	if strings.Contains(first[0], "#m1") || strings.Contains(first[0], "12:01") {
		t.Fatalf("метки не вырезаны из текстовой части: %q", first[0])
	}
	if !strings.Contains(first[0], "Коля: привет") {
		t.Fatalf("текст реплики потерян: %q", first[0])
	}
	// Картинка не должна пострадать: части не текстовые остаются как были.
	parts := msgs[1].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("картинка потеряна при абляции: %v", img)
	}
}

// TestVariantNoPanelInContentParts — панель может жить в текстовой части
// мультимодального последнего user; no-panel обязан её вырезать, иначе
// абляция молча ничего не делает и вывод replay ложный.
func TestVariantNoPanelInContentParts(t *testing.T) {
	body := partsBody()
	markers := PanelMarkers{Open: "[состояние хода]", Close: "[/состояние]"}
	if _, err := ApplyVariants(body, []string{"no-panel"}, markers); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	msgs := body["messages"].([]any)
	text := textParts(t, msgs[3].(map[string]any)["content"])[0]
	if strings.Contains(text, markers.Open) || strings.Contains(text, markers.Close) {
		t.Fatalf("панель осталась в текстовой части: %q", text)
	}
	if !strings.Contains(text, "а ты?") {
		t.Fatalf("реплика потеряна при удалении панели: %q", text)
	}
}

// TestVariantHistoryLastNMultipleSystem — system-сообщений может быть
// несколько; сохраняются все и в исходном порядке, режется только история.
func TestVariantHistoryLastNMultipleSystem(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "основной промпт"},
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "system", "content": "структурные ограничения"},
		map[string]any{"role": "user", "content": "u2"},
		map[string]any{"role": "user", "content": "u3"},
	}}
	if _, err := ApplyVariants(body, []string{"history-last-2"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("сообщений %d, хочу 4 (2 system + 2)", len(msgs))
	}
	roles := make([]string, 0, len(msgs))
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm := m.(map[string]any)
		roles = append(roles, mm["role"].(string))
		texts = append(texts, mm["content"].(string))
	}
	wantRoles := []string{"system", "system", "user", "user"}
	for i := range wantRoles {
		if roles[i] != wantRoles[i] {
			t.Fatalf("роли %v, хочу %v", roles, wantRoles)
		}
	}
	if texts[0] != "основной промпт" || texts[1] != "структурные ограничения" {
		t.Fatalf("system-сообщения потеряны/переставлены: %v", texts)
	}
	if texts[2] != "u2" || texts[3] != "u3" {
		t.Fatalf("хвост истории неверен: %v", texts)
	}
}

// TestVariantHistoryLastNLargerThanHistory — N больше истории: ничего не
// теряется.
func TestVariantHistoryLastNLargerThanHistory(t *testing.T) {
	body := replayBody(t)
	before := len(body["messages"].([]any))
	if _, err := ApplyVariants(body, []string{"history-last-100"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	if got := len(body["messages"].([]any)); got != before {
		t.Fatalf("сообщений %d, хочу %d (история короче N)", got, before)
	}
}

// TestVariantAssistantAsJSONSkipsToolCalls — assistant с tool_calls и
// массивом частей не переупаковывается (это не реплика плейн-текстом):
// абляция не должна ломать tool-цикл в истории.
func TestVariantAssistantAsJSONSkipsToolCalls(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "найди"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "memory_search", "arguments": `{"query":"x"}`}},
		}},
		map[string]any{"role": "tool", "content": "результат"},
		map[string]any{"role": "assistant", "content": "нашёл"},
	}}
	if _, err := ApplyVariants(body, []string{"assistant-as-json"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("сообщений %d, хочу 4", len(msgs))
	}
	withCalls := msgs[1].(map[string]any)
	if _, ok := withCalls["tool_calls"]; !ok {
		t.Fatalf("tool_calls потеряны: %v", withCalls)
	}
	last := msgs[3].(map[string]any)["content"].(string)
	if !strings.HasPrefix(last, `{"decision":"answer"`) {
		t.Fatalf("плейн-ответ не обёрнут: %q", last)
	}
}

// TestVariantNoPanelMissingClose — панель без закрывающего маркера режется до
// конца, а не остаётся в запросе (иначе абляция ложная).
func TestVariantNoPanelMissingClose(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "s"},
		map[string]any{"role": "user", "content": "хвост\n[состояние]\nслужебное без закрытия"},
	}}
	markers := PanelMarkers{Open: "[состояние]", Close: "[/состояние]"}
	if _, err := ApplyVariants(body, []string{"no-panel"}, markers); err != nil {
		t.Fatalf("абляция: %v", err)
	}
	text := body["messages"].([]any)[1].(map[string]any)["content"].(string)
	if strings.Contains(text, markers.Open) || strings.Contains(text, "служебное") {
		t.Fatalf("панель без закрытия осталась: %q", text)
	}
	if text != "хвост" {
		t.Fatalf("текст до панели потерян: %q", text)
	}
}

// TestVariantDegenerateBodies — нет messages/tools, битая температура,
// пустой список абляций: без паник и с явной ошибкой на мусор.
func TestVariantDegenerateBodies(t *testing.T) {
	if _, err := ApplyVariants(map[string]any{"model": "m"}, []string{"no-labels", "no-timestamps", "history-last-2", "assistant-as-json", "merge-assistant", "no-tools"}, PanelMarkers{}); err != nil {
		t.Fatalf("абляции на теле без messages: %v", err)
	}
	for _, bad := range []string{"temperature=abc", "temperature=", "temperature"} {
		if _, err := ApplyVariants(replayBody(t), []string{bad}, PanelMarkers{}); err == nil {
			t.Fatalf("абляция %q: ожидаю ошибку", bad)
		}
	}
	before, _ := json.Marshal(replayBody(t))
	body := replayBody(t)
	if _, err := ApplyVariants(body, nil, PanelMarkers{}); err != nil {
		t.Fatalf("пустой список абляций: %v", err)
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("пустой список абляций изменил запрос")
	}
}
