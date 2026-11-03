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

// TestParseTurnResultInertFieldsBeforeValidate — 17.8: инертные поля
// вычищаются ДО валидации. Ответ со смыслом answer и мусорным defer_hours=0
// раньше валиден по смыслу, но валился проверкой диапазона → repair →
// терминальный пропуск хода. Содержательные нарушения (§11.2) остаются
// терминальными.
func TestParseTurnResultInertFieldsBeforeValidate(t *testing.T) {
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["ок"],"defer_hours":0}`))
	if err != nil {
		t.Fatalf("answer с инертным defer_hours=0 отвергнут: %v", err)
	}
	if r.DeferHours != nil {
		t.Fatalf("defer_hours при answer должен вычищаться: %+v", r.DeferHours)
	}
	// Реакция при не-react/не-answer — инертная, вычищается до валидации.
	r, err = ParseTurnResult([]byte(`{"decision":"ignore","reaction":"👍"}`))
	if err != nil || r.Reaction != "" {
		t.Fatalf("ignore с инертной реакцией должен проходить без reaction: %+v %v", r, err)
	}
	// Содержательные нарушения остаются терминальными.
	if _, err := ParseTurnResult([]byte(`{"decision":"react"}`)); err == nil {
		t.Fatal("react без реакции должен оставаться ошибкой")
	}
	if _, err := ParseTurnResult([]byte(`{"decision":"defer"}`)); err == nil {
		t.Fatal("defer без defer_hours должен оставаться ошибкой")
	}
	// Диапазон defer_hours при самом defer — содержательное нарушение.
	if _, err := ParseTurnResult([]byte(`{"decision":"defer","defer_hours":0}`)); err == nil {
		t.Fatal("defer_hours=0 при defer должен оставаться ошибкой диапазона")
	}
}

// TestParseTurnResultDoubleColonRepair — известный изъян модели: двойное
// двоеточие после ключа чинится до валидации (TS-линия c0a986d).
func TestParseTurnResultDoubleColonRepair(t *testing.T) {
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":: ["ок"]}`))
	if err != nil {
		t.Fatalf("двойное двоеточие не отремонтировано: %v", err)
	}
	if len(r.Messages) != 1 || r.Messages[0] != "ок" {
		t.Fatalf("после ремонта поля прочитаны неверно: %+v", r)
	}
	// Обычный битый JSON по-прежнему ошибка.
	if _, err := ParseTurnResult([]byte(`{"decision":`)); err == nil {
		t.Fatal("обрыв JSON должен оставаться ошибкой")
	}
}

// TestWantForcedTool — force: прямой ход, память есть, первая попытка
// (повторы без forced — thinking-режимы, c0a986d).
func TestWantForcedTool(t *testing.T) {
	if !wantForcedTool(Input{Direct: true, Attempt: 1}, true) {
		t.Fatal("прямой ход, попытка 1 — force нужен")
	}
	if wantForcedTool(Input{Direct: true, Attempt: 2}, true) {
		t.Fatal("повторная попытка — без force")
	}
	if wantForcedTool(Input{Direct: false, Attempt: 1}, true) {
		t.Fatal("фон — без force")
	}
	if wantForcedTool(Input{Direct: true, Attempt: 1}, false) {
		t.Fatal("без инструментов памяти force не имеет смысла")
	}
}

// TestTurnResultAnswerMediaOnly — answer без текста допустим, если доставляет
// стикер или реакцию (2026-11-03: ответ стикером на гифку уходил в скип).
func TestTurnResultAnswerMediaOnly(t *testing.T) {
	cases := []string{
		`{"decision":"answer","messages":[],"sticker":"#4"}`,
		`{"decision":"answer","messages":[],"reaction":"🤣"}`,
		`{"decision":"answer","messages":[],"sticker":"#4","reaction":"🤣"}`,
	}
	for _, raw := range cases {
		r, err := ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("answer без текста, но со стикером/реакцией отвергнут: %v (%s)", err, raw)
		}
		if len(r.Messages) != 0 || (r.Sticker == "" && r.Reaction == "") {
			t.Fatalf("поля прочитаны неверно: %+v", r)
		}
	}
}

// TestTurnResultAnswerWithReaction — необязательная reaction при answer
// допустима (§11.2: «speak содержит 1–4 сообщения; необязательная reaction
// разрешена»); правило «только при react» из TS-реализации противоречило
// спеке — вскрыто живым смоуком 2026-10-30.
func TestTurnResultAnswerWithReaction(t *testing.T) {
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["Ага, понятно"],"reaction":"🤣"}`))
	if err != nil {
		t.Fatalf("answer с реакцией отвергнут: %v", err)
	}
	if r.Decision != DecisionAnswer || r.Reaction != "🤣" || len(r.Messages) != 1 {
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
	raw := `{"decision":"react","reaction":"🤣"}`
	if _, err := ParseTurnResult([]byte(raw)); err != nil {
		t.Fatalf("валидная реакция отвергнута: %v", err)
	}
}

// TestTurnResultReactionAllowlist — reaction валидируется allowlist'ом
// ReactionTypeEmoji Bot API (bug 2026-11-02): мусор «: » не должен доходить до
// Telegram (400 REACTION_INVALID и навсегда failed доставка).
func TestTurnResultReactionAllowlist(t *testing.T) {
	// При answer недопустимая реакция — инертное поле: тихо обнуляется, ход жив.
	r, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["ок"],"reaction":": "}`))
	if err != nil {
		t.Fatalf("answer с мусорной реакцией отвергнут целиком: %v", err)
	}
	if r.Reaction != "" {
		t.Fatalf("мусорная реакция при answer должна обнуляться: %q", r.Reaction)
	}

	// При react реакция — смысл хода: недопустимая — ошибка валидации (repair).
	if _, err := ParseTurnResult([]byte(`{"decision":"react","reaction":": "}`)); err == nil ||
		!strings.Contains(err.Error(), "reaction") {
		t.Fatalf("react с мусорной реакцией не пойман: %v", err)
	}

	// Валидные эмодзи из списка проходят; ❤️ нормализуется к документированному ❤.
	r, err = ParseTurnResult([]byte(`{"decision":"react","reaction":"👀"}`))
	if err != nil || r.Reaction != "👀" {
		t.Fatalf("👀 отвергнут: %+v %v", r, err)
	}
	r, err = ParseTurnResult([]byte(`{"decision":"react","reaction":"❤️"}`))
	if err != nil || r.Reaction != "❤" {
		t.Fatalf("❤️ не нормализован к ❤: %+v %v", r, err)
	}
	// Тон кожи не входит в ReactionTypeEmoji — ошибка, а не тихая отправка.
	if _, err := ParseTurnResult([]byte(`{"decision":"react","reaction":"👍🏽"}`)); err == nil {
		t.Fatal("👍🏽 не должен проходить allowlist")
	}
}

// TestTurnResultSchemaReactionEnum — в JSON-схеме reaction — enum допустимых
// эмодзи (включая оба варианта ❤/❤️), а не свободная строка.
func TestTurnResultSchemaReactionEnum(t *testing.T) {
	props := TurnResultJSONSchema()["properties"].(map[string]any)
	reaction := props["reaction"].(map[string]any)
	enum, ok := reaction["enum"].([]string)
	if !ok || len(enum) == 0 {
		t.Fatalf("в схеме reaction нет enum: %T %v", reaction["enum"], reaction["enum"])
	}
	allowed := map[string]bool{}
	for _, e := range enum {
		allowed[e] = true
	}
	for _, want := range []string{"👀", "❤", "❤️"} {
		if !allowed[want] {
			t.Errorf("enum реакции не содержит %q", want)
		}
	}
	if allowed[":"] || allowed[": "] {
		t.Error("enum реакции содержит мусор \": \"")
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

// TestValidateMessageUTF16Limit — minor 4: 4000 рун из эмодзи вне BMP дают
// больше 4096 единиц UTF-16; Telegram отвечает 400, и ответ теряется. Ловим
// это валидацией до применения, а не в доставке.
func TestValidateMessageUTF16Limit(t *testing.T) {
	tooLong := strings.Repeat("👍", 2100) // 2100 рун, 4200 units UTF-16
	if err := (TurnResult{Decision: DecisionAnswer, Messages: []string{tooLong}}).Validate(); err == nil {
		t.Fatalf("сообщение %d единиц UTF-16 прошло валидацию", utf16Len(tooLong))
	}
	ok := strings.Repeat("👍", 2048) // ровно 4096 units
	if err := (TurnResult{Decision: DecisionAnswer, Messages: []string{ok}}).Validate(); err != nil {
		t.Fatalf("сообщение ровно 4096 units отвергнуто: %v", err)
	}
}

// TestSanitizeInitiativeResult — M4: инициативный ход не создаёт новых
// намерений, defer превращается в ignore (иначе бесконечная цепочка фона).
func TestSanitizeInitiativeResult(t *testing.T) {
	deferHours := 2.0
	defered := TurnResult{
		Decision:   DecisionDefer,
		DeferHours: &deferHours,
		Intentions: []IntentionProposal{{Kind: "explore", Topic: "ещё", Reason: "цепочка", DelayHours: 1}},
	}
	got := SanitizeInitiativeResult(defered)
	if got.Decision != DecisionIgnore || got.DeferHours != nil {
		t.Fatalf("defer не превращён в ignore: %+v", got)
	}
	if len(got.Intentions) != 0 {
		t.Fatalf("намерения инициативы не вычищены: %+v", got.Intentions)
	}

	answer := TurnResult{
		Decision:   DecisionAnswer,
		Messages:   []string{"привет"},
		Intentions: []IntentionProposal{{Kind: "check_in", Topic: "позже", Reason: "x", DelayHours: 1}},
	}
	got = SanitizeInitiativeResult(answer)
	if got.Decision != DecisionAnswer || len(got.Messages) != 1 {
		t.Fatalf("ответ инициативы изменён: %+v", got)
	}
	if len(got.Intentions) != 0 {
		t.Fatalf("намерения answer-инициативы не вычищены: %+v", got.Intentions)
	}
}

// TestParseTurnResultInertOutOfRangeStrippedBeforeValidate — приёмка 17.8:
// инертные поля с ЗНАЧЕНИЯМИ ВНЕ ДИАПАЗОНА вычищаются до валидации — раньше
// именно диапазонная проверка роняла смысл-валидный ответ в repair. Диапазон
// значимого поля при своём decision остаётся терминальным.
func TestParseTurnResultInertOutOfRangeStrippedBeforeValidate(t *testing.T) {
	// react с мусорным defer_hours=999: часы инертны при react.
	r, err := ParseTurnResult([]byte(`{"decision":"react","reaction":"👍","defer_hours":999}`))
	if err != nil {
		t.Fatalf("react с инертным defer_hours=999 отвергнут: %v", err)
	}
	if r.DeferHours != nil {
		t.Fatalf("defer_hours при react должен вычищаться: %+v", r.DeferHours)
	}
	// ignore со сверхдлинной инертной реакцией (лимит 64 руны — проверка длины).
	long := strings.Repeat("👍", maxReactionLen+1)
	r, err = ParseTurnResult([]byte(`{"decision":"ignore","reaction":"` + long + `"}`))
	if err != nil {
		t.Fatalf("ignore со сверхдлинной инертной реакцией отвергнут: %v", err)
	}
	if r.Reaction != "" {
		t.Fatalf("reaction при ignore должен вычищаться: %q", r.Reaction)
	}
	// answer с experience-кандидатом и confidence вне диапазона: confidence
	// осмыслен только у claim — вычищается до валидации, кандидат остаётся.
	r, err = ParseTurnResult([]byte(
		`{"decision":"answer","messages":["ок"],"memory":[{"kind":"experience","text":"прочитал","confidence":5}]}`))
	if err != nil {
		t.Fatalf("experience с confidence=5 отвергнут целиком: %v", err)
	}
	if len(r.Memory) != 1 || r.Memory[0].Confidence != nil {
		t.Fatalf("confidence у experience должен вычищаться до валидации: %+v", r.Memory)
	}
	// Диапазон значимого поля при своём decision — терминален (§11.2).
	if _, err := ParseTurnResult([]byte(`{"decision":"defer","defer_hours":999}`)); err == nil {
		t.Fatal("defer_hours=999 при defer должен оставаться ошибкой диапазона")
	}
}
