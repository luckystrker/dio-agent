// Дополнительные тесты страховки 40.4: ложные срабатывания на нормальных
// репликах (эмодзи, английский текст), пропуски служебной речи и граничные
// случаи перенумерации реплаев.
package character

import (
	"strings"
	"testing"
)

// TestMessageGarbageReasonEmojiOnly — реплика из одного эмодзи — это реплика,
// а не «одна пунктуация». Провайдер не должен терять такие ответы.
func TestMessageGarbageReasonEmojiOnly(t *testing.T) {
	emoji := []string{
		"😀",
		"🔥",
		"❤️",
		"✅",
		"🙂",
		"☕",
		"⭐", // U+2B50 — эмодзи-презентация по умолчанию, без FE0F
		"⏰", // U+23F0
		"⌛", // U+231B
	}
	for _, text := range emoji {
		if reason := MessageGarbageReason(text); reason != "" {
			t.Fatalf("эмодзи %q признано браком: %s", text, reason)
		}
	}
}

// TestMessageGarbageReasonEnglishNotGarbage — имена полей схемы — английские
// слова, но нормальная английская реплика не должна выкидываться: детектор
// обязан отличать токен формата от живой речи.
func TestMessageGarbageReasonEnglishNotGarbage(t *testing.T) {
	normal := []string{
		"no reason to worry",
		"send me notes later",
		"I read your messages",
		"my reaction was fine",
		"forward it to me",
	}
	for _, text := range normal {
		if reason := MessageGarbageReason(text); reason != "" {
			t.Fatalf("английская реплика %q признана браком: %s", text, reason)
		}
	}
}

// TestMessageGarbageReasonMetaWithoutSchemaWord — модель может сказать
// «Укажи JSON» и без слова «схема»: это такая же служебная речь, как живая
// строка 246 «Укажи JSON по схеме Turn».
func TestMessageGarbageReasonMetaWithoutSchemaWord(t *testing.T) {
	for _, text := range []string{"Укажи JSON", "верни JSON", "JSON only"} {
		if reason := MessageGarbageReason(text); reason == "" {
			t.Fatalf("служебная речь %q пропущена детектором", text)
		}
	}
}

// TestMessageGarbageReasonEmptyAndFragments — пустое сообщение и обрывки JSON
// (начало/конец/ряды знаков, регистр полей) — брак.
func TestMessageGarbageReasonEmptyAndFragments(t *testing.T) {
	garbage := map[string]string{
		"":                 "пустое",
		"   \n\t":          "пустое",
		"{":                "пунктуация",
		`{"decision":"x"`:  "decision",
		"привет {":         "обрывок",
		"хвост }":          "обрывок",
		`"key":":`:         "обрывок",
		"ответ }}}]":       "ряд JSON",
		"MESSAGES":         "MESSAGES",
		"Decision: answer": "Decision",
		"mood_delta и всё": "mood_delta",
	}
	for text, want := range garbage {
		reason := MessageGarbageReason(text)
		if reason == "" {
			t.Fatalf("брак не найден: %q", text)
		}
		if !strings.Contains(reason, want) {
			t.Fatalf("причина для %q = %q, хочу %q", text, reason, want)
		}
	}
}

// TestDropGarbageMessagesRemapReplies — реплаи переносятся на новые номера:
// реплай выкинутого сообщения исчезает, реплай вне диапазона не воскресает.
func TestDropGarbageMessagesRemapReplies(t *testing.T) {
	r := TurnResult{
		Decision: DecisionAnswer,
		Messages: []string{",", "первое", "второе"},
		ReplyToByMessage: []MessageReply{
			{Index: 1, Ref: "#m1"}, // выкинутое сообщение
			{Index: 2, Ref: "#m2"}, // станет 1
			{Index: 3, Ref: "#m3"}, // станет 2
			{Index: 5, Ref: "#m5"}, // вне диапазона
		},
	}
	got, dropped := dropGarbageMessages(r)
	if len(dropped) != 1 {
		t.Fatalf("выкинуто %d (%v), хочу 1", len(dropped), dropped)
	}
	if len(got.ReplyToByMessage) != 2 {
		t.Fatalf("реплаи = %+v, хочу два (на оставшиеся сообщения)", got.ReplyToByMessage)
	}
	if got.ReplyToByMessage[0].Index != 1 || got.ReplyToByMessage[0].Ref != "#m2" {
		t.Fatalf("первый реплай не перенумерован: %+v", got.ReplyToByMessage[0])
	}
	if got.ReplyToByMessage[1].Index != 2 || got.ReplyToByMessage[1].Ref != "#m3" {
		t.Fatalf("второй реплай не перенумерован: %+v", got.ReplyToByMessage[1])
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("результат после чистки невалиден: %v", err)
	}
}

// TestDropGarbageMessagesOnlyAnswer — функция трогает только answer: react/
// ignore с сообщениями остаются как есть (их разбирает валидация схемы).
func TestDropGarbageMessagesOnlyAnswer(t *testing.T) {
	for _, decision := range []string{DecisionReact, DecisionIgnore, DecisionDefer} {
		r := TurnResult{Decision: decision, Messages: []string{","}}
		got, dropped := dropGarbageMessages(r)
		if len(dropped) != 0 {
			t.Fatalf("%s: выкинуто %v, хочу ничего", decision, dropped)
		}
		if len(got.Messages) != 1 || got.Messages[0] != "," {
			t.Fatalf("%s: сообщения изменены: %v", decision, got.Messages)
		}
	}
}

// TestDropGarbageMessagesStickerOnlyDegradesToReact — все сообщения брак, но
// есть стикер: деградация в react, стикер сохраняется (38.2).
func TestDropGarbageMessagesStickerOnlyDegradesToReact(t *testing.T) {
	got, dropped := dropGarbageMessages(TurnResult{
		Decision: DecisionAnswer,
		Messages: []string{",", "}"},
		Sticker:  "#6",
	})
	if len(dropped) != 2 {
		t.Fatalf("выкинуто %d, хочу 2", len(dropped))
	}
	if got.Decision != DecisionReact || got.Sticker != "#6" || len(got.Messages) != 0 {
		t.Fatalf("ожидал react со стикером: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("деградированный результат невалиден: %v", err)
	}
}

// TestDropGarbageMessagesKeepsValidOnly — если брака нет, результат не
// меняется и реплаи не пересобираются.
func TestDropGarbageMessagesKeepsValidOnly(t *testing.T) {
	r := TurnResult{
		Decision:         DecisionAnswer,
		Messages:         []string{"привет", "как ты?"},
		ReplyToByMessage: []MessageReply{{Index: 2, Ref: "#m2"}},
	}
	got, dropped := dropGarbageMessages(r)
	if len(dropped) != 0 {
		t.Fatalf("выкинуто %v при здоровых сообщениях", dropped)
	}
	if len(got.Messages) != 2 || got.ReplyToByMessage[0].Index != 2 {
		t.Fatalf("здоровый результат изменён: %+v", got)
	}
}
