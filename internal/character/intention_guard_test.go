// Юнит-тесты страховок отложенных намерений (26.2): единицы delay_hours в
// подсказках модели, runtime-пол задержки по виду и отсев темы-мусора.
package character

import (
	"strings"
	"testing"
)

// TestFloorIntentionDelay — runtime-пол по виду намерения: модель путает часы
// с минутами (боевые данные check_in 0.094 ч = 5.6 минуты), runtime поднимает
// задержку до пола, а не отклоняет намерение.
func TestFloorIntentionDelay(t *testing.T) {
	cases := []struct {
		kind   string
		in     float64
		want   float64
		raised bool
	}{
		{"check_in", 0.094, 2, true},
		{"check_in", 2, 2, false},
		{"check_in", 5, 5, false},
		{"share", 0.069, 0.25, true},
		{"share", 0.3, 0.3, false},
		{"reply_later", 0.078, 0.1, true},
		{"reply_later", 0.1, 0.1, false},
		{"explore", 0.05, 0.05, false},
		{"explore", 10.0 / 60.0, 10.0 / 60.0, false},
		{"неизвестное", 0.01, 0.01, false},
	}
	for _, c := range cases {
		got, raised := floorIntentionDelay(c.kind, c.in)
		if got != c.want || raised != c.raised {
			t.Errorf("floorIntentionDelay(%q, %v) = (%v, %v), хочу (%v, %v)",
				c.kind, c.in, got, raised, c.want, c.raised)
		}
	}
}

// TestGarbageIntentionTopic — тема-мусор отклоняется как дубль (§14.1): ход
// живёт, намерение не создаётся. Мусор — пустая тема, тема без букв (эмодзи,
// пунктуация), шаблоны-плейсхолдеры.
func TestGarbageIntentionTopic(t *testing.T) {
	garbage := []string{
		"",
		"   ",
		"placeholder1",
		"PlaceHolder",
		"todo",
		"todo: позвонить",
		"тема",
		"Тема",
		"topic",
		"xxx",
		"XXX",
		":)",
		"???",
		"😂😂",
		"!!! 123",
	}
	for _, topic := range garbage {
		if reason := garbageTopicReason(topic); reason == "" {
			t.Errorf("тема %q не распознана как мусор", topic)
		}
	}

	good := []string{
		"эпилепсия",
		"  эпилепсия  ",
		"как ты там",
		"планы на выходные",
		"разговор о пластинках",
		"тема дня",
	}
	for _, topic := range good {
		if reason := garbageTopicReason(topic); reason != "" {
			t.Errorf("тема %q ошибочно отклонена: %s", topic, reason)
		}
	}
}

// TestDelayHoursHintInConstraints — модель должна видеть единицы измерения
// там, где она выбирает задержку: в constraints хода и описании JSON-схемы.
func TestDelayHoursHintInConstraints(t *testing.T) {
	if !strings.Contains(constraintsSection(), "в ЧАСАХ") {
		t.Fatalf("constraints хода не объясняют delay_hours в часах:\n%s", constraintsSection())
	}
	if !strings.Contains(constraintsSection(), "0.5 = 30 минут") {
		t.Fatalf("constraints хода не дают примера 0.5 = 30 минут:\n%s", constraintsSection())
	}

	schema := TurnResultJSONSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("у схемы нет properties: %T", schema["properties"])
	}
	intentions, ok := props["intentions"].(map[string]any)
	if !ok {
		t.Fatalf("у схемы нет intentions: %T", props["intentions"])
	}
	items, ok := intentions["items"].(map[string]any)
	if !ok {
		t.Fatalf("у intentions нет items: %T", intentions["items"])
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("у items нет properties: %T", items["properties"])
	}
	delay, ok := itemProps["delay_hours"].(map[string]any)
	if !ok {
		t.Fatalf("в схеме нет delay_hours: %T", itemProps["delay_hours"])
	}
	desc, _ := delay["description"].(string)
	if !strings.Contains(desc, "в ЧАСАХ") || !strings.Contains(desc, "0.5 = 30 минут") {
		t.Fatalf("описание delay_hours в схеме не объясняет единицы: %q", desc)
	}
}
