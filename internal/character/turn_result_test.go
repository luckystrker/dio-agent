// Юнит-тесты TurnResult §11.2: правила действий, лимиты длин, дельты с
// основаниями, строгий JSON. Поведение — src/character/turn-result.ts.
package character

import (
	"encoding/json"
	"strings"
	"testing"
)

func validAnswerJSON() string {
	return `{"decision":"answer","messages":["Привет"],"memory":[{"kind":"claim","text":"любит дождь","entities":[{"name":"Коля","kind":"person","role":"subject"}]}],"mood_delta":{"valence":0.1,"reason":"приятная новость"}}`
}

func TestParseTurnResultValid(t *testing.T) {
	r, err := ParseTurnResult([]byte(validAnswerJSON()))
	if err != nil {
		t.Fatalf("валидный результат отвергнут: %v", err)
	}
	if r.Decision != DecisionAnswer || len(r.Messages) != 1 || r.Messages[0] != "Привет" {
		t.Fatalf("поля прочитаны неверно: %+v", r)
	}
	if len(r.Memory) != 1 || r.Memory[0].Kind != "claim" || r.Memory[0].Entities[0].Name != "Коля" {
		t.Fatalf("кандидат памяти прочитан неверно: %+v", r.Memory)
	}
	if r.MoodDelta == nil || r.MoodDelta.Reason != "приятная новость" {
		t.Fatalf("дельта настроения прочитана неверно: %+v", r.MoodDelta)
	}
}

func TestParseTurnResultStrictJSON(t *testing.T) {
	// Неизвестное поле запрещено (additionalProperties: false).
	raw := `{"decision":"ignore","notes":[],"whatever":1}`
	_, err := ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "whatever") {
		t.Fatalf("неизвестное поле не поймано: %v", err)
	}
}

func TestTurnResultDecisionRules(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"answer без сообщений", `{"decision":"answer","messages":[]}`, "answer требует"},
		{"react без реакции", `{"decision":"react"}`, "react требует reaction"},
		{"react с сообщениями", `{"decision":"react","reaction":"👍","messages":["текст"]}`, "react не содержит сообщений"},
		{"ignore с сообщениями", `{"decision":"ignore","messages":["текст"]}`, "не содержат отправляемых сообщений"},
		{"defer с сообщениями", `{"decision":"defer","defer_hours":1,"messages":["текст"]}`, "не содержат отправляемых сообщений"},
		{"defer без часов", `{"decision":"defer"}`, "defer требует defer_hours"},
		{"неизвестный decision", `{"decision":"shout"}`, "неизвестное decision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTurnResult([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("хочу ошибку %q, получено: %v", tc.want, err)
			}
		})
	}
}

// TestTurnResultStripsInertContradictions — инертные противоречия (поля без
// эффекта при выбранном decision) вычищаются, а не роняют ход (§11.3: runtime
// может мягко подавить часть результата): модели слабых провайдеров стабильно
// кладут мусор в необязательные поля — вскрыто живым смоуком 2026-10-30.
func TestTurnResultStripsInertContradictions(t *testing.T) {
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["Ага"],"defer_hours":2}`))
	if err != nil {
		t.Fatalf("answer с посторонним defer_hours отвергнут: %v", err)
	}
	if r.DeferHours != nil {
		t.Fatalf("defer_hours при answer должен вычищаться: %+v", r.DeferHours)
	}
	r, err = ParseTurnResult([]byte(`{"decision":"ignore","reaction":"👍"}`))
	if err != nil {
		t.Fatalf("ignore с посторонней реакцией отвергнут: %v", err)
	}
	if r.Reaction != "" {
		t.Fatalf("reaction при ignore должен вычищаться: %+v", r.Reaction)
	}
	r, err = ParseTurnResult([]byte(`{"decision":"defer","defer_hours":1,"reaction":"👍"}`))
	if err != nil {
		t.Fatalf("defer с посторонней реакцией отвергнут: %v", err)
	}
	if r.Reaction != "" || r.DeferHours == nil {
		t.Fatalf("нормализация defer неверна: %+v", r)
	}
}

// TestTurnResultStripsConfidenceOffClaim — confidence у не-claim кандидата
// памяти вычищается, кандидат сохраняется (§11.3): живой прогон 2026-10-31 —
// модель дважды положила confidence в experience, ход тонул целиком.
func TestTurnResultStripsConfidenceOffClaim(t *testing.T) {
	r, err := ParseTurnResult([]byte(
		`{"decision":"answer","messages":["ок"],"memory":[{"kind":"experience","text":"прочитал статью","confidence":0.7}]}`))
	if err != nil {
		t.Fatalf("experience с confidence отвергнут целиком: %v", err)
	}
	if len(r.Memory) != 1 || r.Memory[0].Confidence != nil {
		t.Fatalf("confidence у experience должен вычищаться, кандидат оставаться: %+v", r.Memory)
	}
	// У claim confidence остаётся.
	r, err = ParseTurnResult([]byte(
		`{"decision":"answer","messages":["ок"],"memory":[{"kind":"claim","text":"источник говорит X","confidence":0.5}]}`))
	if err != nil || r.Memory[0].Confidence == nil {
		t.Fatalf("confidence у claim не должен вычищаться: %+v %v", r.Memory, err)
	}
}

// TestTurnResultAnswerWithReaction — необязательная reaction при answer
// допустима (§11.2: «speak содержит 1–4 сообщения; необязательная reaction
// разрешена»); правило «только при react» из TS-реализации противоречило
// спеке — вскрыто живым смоуком 2026-10-30.
func TestTurnResultAnswerWithReaction(t *testing.T) {
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["Ага, понятно"],"reaction":"😂"}`))
	if err != nil {
		t.Fatalf("answer с реакцией отвергнут: %v", err)
	}
	if r.Decision != DecisionAnswer || r.Reaction != "😂" || len(r.Messages) != 1 {
		t.Fatalf("поля прочитаны неверно: %+v", r)
	}
}

func TestTurnResultLengthLimits(t *testing.T) {
	long := strings.Repeat("ы", maxMessageLength+1)
	raw := `{"decision":"answer","messages":["` + long + `"]}`
	if _, err := ParseTurnResult([]byte(raw)); err == nil || !strings.Contains(err.Error(), "4000") {
		t.Fatalf("сообщение длиннее 4000 не поймано: %v", err)
	}

	many := make([]string, maxMessages+1)
	for i := range many {
		many[i] = "x"
	}
	rawBytes, merr := json.Marshal(many)
	if merr != nil {
		t.Fatalf("массив не сериализуется: %v", merr)
	}
	raw = `{"decision":"answer","messages":` + string(rawBytes) + `}`
	if _, err := ParseTurnResult([]byte(raw)); err == nil || !strings.Contains(err.Error(), "не более 4 сообщений") {
		t.Fatalf("пять сообщений не пойманы: %v", err)
	}

	raw = `{"decision":"ignore","notes":["` + strings.Repeat("ы", maxNoteLength+1) + `"]}`
	if _, err := ParseTurnResult([]byte(raw)); err == nil || !strings.Contains(err.Error(), "2000") {
		t.Fatalf("заметка длиннее 2000 не поймана: %v", err)
	}
}

func TestTurnResultDeltasRequireReason(t *testing.T) {
	raw := `{"decision":"ignore","mood_delta":{"valence":0.5}}`
	_, err := ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "основание") {
		t.Fatalf("дельта без основания не поймана: %v", err)
	}
	raw = `{"decision":"ignore","relationship_delta":{"trust":0.5}}`
	_, err = ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "основание") {
		t.Fatalf("дельта отношений без основания не поймана: %v", err)
	}
}

func TestTurnResultDeltaRanges(t *testing.T) {
	raw := `{"decision":"ignore","mood_delta":{"valence":2,"reason":"r"}}`
	_, err := ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "valence") {
		t.Fatalf("valence вне [-1..1] не пойман: %v", err)
	}
	raw = `{"decision":"ignore","relationship_delta":{"trust":1.5,"reason":"r"}}`
	_, err = ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "trust") {
		t.Fatalf("trust вне [0..1] не пойман: %v", err)
	}
}

func TestTurnResultMemoryRules(t *testing.T) {
	// confidence у не-claim вычищается, кандидат живёт (§11.3; решение
	// 2026-10-31 — см. TestTurnResultStripsConfidenceOffClaim).
	raw := `{"decision":"ignore","memory":[{"kind":"thought","text":"мысль","confidence":0.9}]}`
	r, err := ParseTurnResult([]byte(raw))
	if err != nil {
		t.Fatalf("thought с confidence отвергнут целиком: %v", err)
	}
	if r.Memory[0].Confidence != nil {
		t.Fatalf("confidence у мысли должен вычищаться: %+v", r.Memory[0])
	}
	// Неизвестный kind.
	raw = `{"decision":"ignore","memory":[{"kind":"rumor","text":"x"}]}`
	_, err = ParseTurnResult([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("неизвестный kind памяти не пойман: %v", err)
	}
	// Лимит 5 кандидатов.
	var sb strings.Builder
	sb.WriteString(`{"decision":"ignore","memory":[`)
	for i := 0; i < maxMemories+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"kind":"claim","text":"x"}`)
	}
	sb.WriteString(`]}`)
	_, err = ParseTurnResult([]byte(sb.String()))
	if err == nil || !strings.Contains(err.Error(), "кандидатов памяти") {
		t.Fatalf("шесть кандидатов не пойманы: %v", err)
	}
}

func TestTurnResultDeferredValidPath(t *testing.T) {
	raw := `{"decision":"defer","defer_hours":2,"notes":["вернусь позже"],"intentions":[{"kind":"reply_later","topic":"кино","reason":"не готов ответить","delay_hours":2}]}`
	r, err := ParseTurnResult([]byte(raw))
	if err != nil {
		t.Fatalf("валидный defer отвергнут: %v", err)
	}
	if len(r.Intentions) != 1 || r.Intentions[0].Kind != "reply_later" {
		t.Fatalf("намерение прочитано неверно: %+v", r.Intentions)
	}
}

func TestTurnResultReactValidPath(t *testing.T) {
	raw := `{"decision":"react","reaction":"😂"}`
	if _, err := ParseTurnResult([]byte(raw)); err != nil {
		t.Fatalf("валидная реакция отвергнута: %v", err)
	}
}

// Согласованность схемы и валидатора: любой JSON, который валидатор принял,
// обязан проходить по форме JSON-схемы (поля и лимиты из одних констант —
// выборочная сверка ключевых точек).
func TestSchemaMatchesValidatorLimits(t *testing.T) {
	schema := TurnResultJSONSchema()
	props := schema["properties"].(map[string]any)

	msgs := props["messages"].(map[string]any)
	if msgs["maxItems"] != maxMessages {
		t.Fatalf("схема допускает %v сообщений, валидатор — %d", msgs["maxItems"], maxMessages)
	}
	if msgs["items"].(map[string]any)["maxLength"] != maxMessageLength {
		t.Fatalf("схема: maxLength сообщения не совпадает с валидатором")
	}
	notes := props["notes"].(map[string]any)
	if notes["maxItems"] != maxNotes {
		t.Fatalf("схема допускает %v заметок, валидатор — %d", notes["maxItems"], maxNotes)
	}
	memorySchema := props["memory"].(map[string]any)
	if memorySchema["maxItems"] != maxMemories {
		t.Fatalf("схема допускает %v кандидатов памяти, валидатор — %d", memorySchema["maxItems"], maxMemories)
	}
	if got := props["decision"].(map[string]any)["enum"].([]string); len(got) != 4 {
		t.Fatalf("в схеме %d действий, хочу 4", len(got))
	}
	mood := props["mood_delta"].(map[string]any)
	if req := mood["required"].([]string); len(req) != 1 || req[0] != "reason" {
		t.Fatalf("схема mood_delta требует %v, валидатор требует reason", req)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("схема допускает лишние поля, валидатор — нет")
	}
}
