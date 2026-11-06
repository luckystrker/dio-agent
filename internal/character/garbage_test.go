// Тесты детектора брака в messages (40.4): чистый функция, живая таблица из
// ходов 246–248 и набор нормальных сообщений, на которых детектор молчит.
package character

import (
	"fmt"
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
		{"sticker: #3", "sticker"},
		{"#3", "метка стикера"},
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

// TestDropGarbageMessagesClearsForwardOnReact — m8 ревью: при деградации в
// react пересылка/реплаи к выкинутым сообщениям не должны пережить ход.
func TestDropGarbageMessagesClearsForwardOnReact(t *testing.T) {
	got, _ := dropGarbageMessages(TurnResult{
		Decision:         DecisionAnswer,
		Messages:         []string{",", "],"},
		Reaction:         "🤔",
		Forward:          "#m5",
		ReplyToByMessage: []MessageReply{{Index: 1, Ref: "#m1"}},
	})
	if got.Decision != DecisionReact || got.Forward != "" || len(got.ReplyToByMessage) != 0 {
		t.Fatalf("react сохранил пересылку/реплаи: %+v", got)
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

// TestMessageGarbageReasonNoFalsePositives — живая речь с запятыми, повторным
// словом и техническим упоминанием JSON не должна выкидываться (находка
// критика: страховка не имеет права терять валидный ответ).
func TestMessageGarbageReasonNoFalsePositives(t *testing.T) {
	normal := []string{
		"I have a bad memory, you know",
		"See the notes, please",
		"Мой reaction, если честно, спокойный",
		"Мне нравится формат JSON",
		"Send me the JSON later",
		"ты очень kind, спасибо",
		"я не помню, что было в messages, но ладно", // одно поле в живой речи
		// m1 ревью: «по схеме» и императив+JSON — штатная речь про код.
		"сделал по схеме из доки",
		"всё собрал по схеме, работает",
		"верни JSON из функции",
		"выведи JSON в лог",
		"напиши JSON конфига",
		// m2 ревью: капс — акцент, а не формат.
		"TRUST ME",
	}
	for _, text := range normal {
		if reason := MessageGarbageReason(text); reason != "" {
			t.Fatalf("ложное срабатывание на %q: %s", text, reason)
		}
	}
}

// TestMessageGarbageReasonCodeNotGarbage — M2 ревью: ответы с кодом не должны
// чиниться/выкидываться страховкой. Скобки и двоеточия сами по себе — не
// признак скопированного JSON.
func TestMessageGarbageReasonCodeNotGarbage(t *testing.T) {
	code := []string{
		"func main() {",
		"if err != nil {",
		"x = {a: 1}",
		"return nil }",
		"[[1],[2]]",
		"for i := range xs {\n\tfmt.Println(i)\n}",
		"type Config struct {\n\tName string\n}",
		`fmt.Printf("%v: %v", key, value)`,
		`{"a": 1} — примерно так выглядит конфиг`,
	}
	for _, text := range code {
		if reason := MessageGarbageReason(text); reason != "" {
			t.Fatalf("код %q признан браком: %s", text, reason)
		}
	}
}

// TestMessageGarbageReasonStandaloneFieldsAndImperatives — сообщение из одного
// имени поля и приказ себе «выведи/напиши JSON» — брак: это скопированный
// формат, а не реплика.
func TestMessageGarbageReasonStandaloneFieldsAndImperatives(t *testing.T) {
	garbage := []string{
		"messages", "decision", "sticker", "notes", "reason",
		"delay_hours", "valence",
		"Выведи JSON", "Сгенерируй JSON", "Напиши JSON", "Ответь JSON", "Отдай JSON",
		"Верни по схеме Turn",
	}
	for _, text := range garbage {
		if reason := MessageGarbageReason(text); reason == "" {
			t.Fatalf("брак не найден: %q", text)
		}
	}
}

// TestFixGarbageMessagesKeepsFirstOnRepairFailure — сбой repair не роняет ход
// и не теряет чистые реплики первого ответа.
func TestFixGarbageMessagesKeepsFirstOnRepairFailure(t *testing.T) {
	first := TurnResult{Decision: DecisionAnswer, Messages: []string{",", "привет"}}
	got, repairs, dropped := FixGarbageMessages(first, true, func() (TurnResult, error) {
		return TurnResult{}, fmt.Errorf("сеть")
	})
	if repairs != 0 {
		t.Fatalf("repair посчитан (%d) при ошибке", repairs)
	}
	if len(got.Messages) != 1 || got.Messages[0] != "привет" || len(dropped) != 1 {
		t.Fatalf("чистые реплики первого ответа потеряны: %+v (%v)", got, dropped)
	}
}

// TestFixGarbageMessagesPrefersCleanRepair — repair с чистым answer принимается.
func TestFixGarbageMessagesPrefersCleanRepair(t *testing.T) {
	first := TurnResult{Decision: DecisionAnswer, Messages: []string{">»"}}
	got, repairs, dropped := FixGarbageMessages(first, true, func() (TurnResult, error) {
		return TurnResult{Decision: DecisionAnswer, Messages: []string{"я тут"}}, nil
	})
	if repairs != 1 || len(got.Messages) != 1 || got.Messages[0] != "я тут" || len(dropped) != 0 {
		t.Fatalf("чистый repair не принят: %+v repairs=%d dropped=%v", got, repairs, dropped)
	}
}

// TestFixGarbageMessagesKeepsFirstWhenRepairWorse — repair вернул ignore (или
// мусор): не теряем валидные реплики первого ответа.
func TestFixGarbageMessagesKeepsFirstWhenRepairWorse(t *testing.T) {
	first := TurnResult{Decision: DecisionAnswer, Messages: []string{",", "как ты?"}}
	got, repairs, _ := FixGarbageMessages(first, true, func() (TurnResult, error) {
		return TurnResult{Decision: DecisionIgnore}, nil
	})
	if repairs != 0 {
		t.Fatalf("repair посчитан (%d), хотя он не принят", repairs)
	}
	if len(got.Messages) != 1 || got.Messages[0] != "как ты?" {
		t.Fatalf("первый ответ потерян: %+v", got)
	}
}
