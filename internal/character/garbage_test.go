// Тесты детектора брака в messages (40.4): чистый функция, живая таблица из
// ходов 246–248 и набор нормальных сообщений, на которых детектор молчит.
package character

import (
	"strings"
	"testing"
)

func TestMessageGarbageReasonLive(t *testing.T) {
	cases := []struct {
		text string
		want string // подстрока причины; пусто — не брак
	}{
		// Живые строки ходов 246–248 (этап 40, CHECKLIST).
		{",", "пунктуация"},
		{">»", "пунктуация"},
		{"…", "пунктуация"},
		{"messagesекафедра", "messages"},
		{"mood_delta},", "mood_delta"},
		{"reply_to_by_message:,", "reply_to_by_message"},
		{"я думаю вылидaть #m1034 «…» #m1035 «…»", "#m"},
		{"}",
			"пунктуация"},
		{"Укажи JSON по схеме Turn", "JSON"},
		{"ответ }", "обрывок"},
		{"sticker: #3", "sticker"},
	}
	for _, tc := range cases {
		got := MessageGarbageReason(tc.text)
		if got == "" {
			t.Fatalf("брак не найден: %q", tc.text)
		}
		if !strings.Contains(got, tc.want) {
			t.Fatalf("причина для %q = %q, хочу упоминание %q", tc.text, got, tc.want)
		}
	}
}

func TestMessageGarbageReasonNormal(t *testing.T) {
	normal := []string{
		"ок",
		"бля",
		"да?",
		":)",
		")))",
		"😂😂",
		"Привет! Как дела?",
		"ну и ладно…",
		"[фото] смотри, какой закат",
		"я думаю, что это плохая идея",
		"Ок, давай в 7",
		"Спасибо, что напомнил про встречу!",
		"«Ну да» — ответил он.",
		"не, я про другое",
		"Ага",
		"давай завтра, я устал",
		"#3", // метка стикера в поле sticker — не текст сообщения
	}
	for _, text := range normal {
		if reason := MessageGarbageReason(text); reason != "" {
			t.Fatalf("ложное срабатывание на нормальном %q: %s", text, reason)
		}
	}
}

// TestDropGarbageMessages — после repair бракованные сообщения выкидываются,
// остальные уходят; реплаи перенумеровываются под новый список.
func TestDropGarbageMessages(t *testing.T) {
	r := TurnResult{
		Decision: DecisionAnswer,
		Messages: []string{",", "Привет, как ты?", "reply_to_by_message:,"},
		ReplyToByMessage: []MessageReply{
			{Index: 1, Ref: "#m1"},
			{Index: 2, Ref: "#m2"},
			{Index: 3, Ref: "#m3"},
		},
	}
	got, dropped := dropGarbageMessages(r)
	if len(dropped) != 2 {
		t.Fatalf("выкинуто %d сообщений (%v), хочу 2", len(dropped), dropped)
	}
	if len(got.Messages) != 1 || got.Messages[0] != "Привет, как ты?" {
		t.Fatalf("остались не те сообщения: %v", got.Messages)
	}
	if len(got.ReplyToByMessage) != 1 || got.ReplyToByMessage[0].Index != 1 || got.ReplyToByMessage[0].Ref != "#m2" {
		t.Fatalf("реплаи не перенумерованы: %+v", got.ReplyToByMessage)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("результат после чистки невалиден: %v", err)
	}
}

// TestDropGarbageMessagesAllBadDegrades — все сообщения брак: деградация как
// 38.2 (react при реакции/стикере, иначе ignore).
func TestDropGarbageMessagesAllBadDegrades(t *testing.T) {
	r := TurnResult{Decision: DecisionAnswer, Messages: []string{",", "},"}, Reaction: "🤔"}
	got, dropped := dropGarbageMessages(r)
	if len(dropped) != 2 {
		t.Fatalf("выкинуто %d, хочу 2", len(dropped))
	}
	if got.Decision != DecisionReact || len(got.Messages) != 0 || got.Reaction != "🤔" {
		t.Fatalf("деградация не в react с реакцией: %+v", got)
	}

	r2 := TurnResult{Decision: DecisionAnswer, Messages: []string{">»"}}
	got2, _ := dropGarbageMessages(r2)
	if got2.Decision != DecisionIgnore || len(got2.Messages) != 0 {
		t.Fatalf("без реакции деградация должна быть в ignore: %+v", got2)
	}
}

// TestGarbageRepairHint — подсказка прямо говорит: messages — только текст
// для собеседника, без меток и служебных полей.
func TestGarbageRepairHint(t *testing.T) {
	for _, want := range []string{"messages", "только текст", "без меток", "служебных полей"} {
		if !strings.Contains(garbageRepairHint, want) {
			t.Fatalf("в подсказке нет %q:\n%s", want, garbageRepairHint)
		}
	}
}
