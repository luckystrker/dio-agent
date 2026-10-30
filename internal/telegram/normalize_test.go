// Юнит-тесты нормализации update (§6.1, шаг 1). Поведение —
// src/telegram/normalize.ts (+ PBT-инварианты normalize.pbt.test.ts этапа 2
// как табличные примеры).
package telegram

import (
	"encoding/json"
	"testing"
)

func mustNormalize(t *testing.T, raw string) NormalizedEvent {
	t.Helper()
	event, err := NormalizeUpdate(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("нормализация не удалась: %v", err)
	}
	return event
}

func TestNormalizePrivateMessage(t *testing.T) {
	event := mustNormalize(t, `{
		"update_id": 100,
		"message": {
			"message_id": 7,
			"from": {"id": 42, "first_name": "Коля"},
			"chat": {"id": 42, "type": "private"},
			"text": "привет"
		}
	}`)
	if event.TelegramUpdateID != 100 || event.ChatID != 42 || event.ChatType != ChatPrivate {
		t.Fatalf("неверные id/тип: %+v", event)
	}
	if event.Kind != KindMessage || !event.Supported {
		t.Fatalf("неверный kind/supported: %+v", event)
	}
	if event.ActorID == nil || *event.ActorID != 42 {
		t.Fatalf("актор не извлечён: %+v", event)
	}
	if event.ActorName == nil || *event.ActorName != "Коля" {
		t.Fatalf("имя актора: %+v", event.ActorName)
	}
	if event.Text == nil || *event.Text != "привет" {
		t.Fatalf("текст: %+v", event.Text)
	}
}

func TestNormalizeDetectsKinds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want EventKind
	}{
		{"reply", `{"update_id":1,"message":{"message_id":1,"chat":{"id":-5,"type":"group"},"reply_to_message":{"message_id":9},"text":"ага"}}`, KindReply},
		{"command", `{"update_id":2,"message":{"message_id":1,"chat":{"id":-5,"type":"group"},"entities":[{"type":"bot_command","offset":0,"length":5}],"text":"/start"}}`, KindCommand},
		{"mention", `{"update_id":3,"message":{"message_id":1,"chat":{"id":-5,"type":"supergroup"},"entities":[{"type":"mention","offset":0,"length":6}],"text":"@dio_bot"}}`, KindMention},
		{"edited", `{"update_id":4,"edited_message":{"message_id":1,"chat":{"id":-5,"type":"group"},"text":"поправка"}}`, KindEdited},
		{"channel_post", `{"update_id":5,"channel_post":{"message_id":1,"chat":{"id":-100,"type":"channel"},"text":"новость"}}`, KindMessage},
		{"plain group", `{"update_id":6,"message":{"message_id":1,"chat":{"id":-5,"type":"group"},"text":"шум"}}`, KindMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustNormalize(t, tc.raw).Kind; got != tc.want {
				t.Fatalf("kind = %s, хочу %s", got, tc.want)
			}
		})
	}
}

func TestNormalizeActorNameFallbacks(t *testing.T) {
	full := mustNormalize(t, `{"update_id":1,"message":{"from":{"id":1,"first_name":"А","last_name":"Б"},"chat":{"id":1,"type":"private"}}}`)
	if full.ActorName == nil || *full.ActorName != "А Б" {
		t.Fatalf("имя и фамилия: %+v", full.ActorName)
	}
	user := mustNormalize(t, `{"update_id":2,"message":{"from":{"id":1,"username":"kolya"},"chat":{"id":1,"type":"private"}}}`)
	if user.ActorName == nil || *user.ActorName != "kolya" {
		t.Fatalf("username: %+v", user.ActorName)
	}
	none := mustNormalize(t, `{"update_id":3,"message":{"chat":{"id":1,"type":"private"}}}`)
	if none.ActorName != nil || none.ActorID != nil {
		t.Fatalf("без from актора быть не должно: %+v", none)
	}
}

func TestNormalizeUnsupportedUpdate(t *testing.T) {
	// callback_query не поддерживается: журнал без обработки, offset не
	// застревает (§6.1).
	event := mustNormalize(t, `{"update_id":50,"callback_query":{"id":"x","data":"y"}}`)
	if event.Supported {
		t.Fatalf("callback_query не должен быть supported: %+v", event)
	}
	if event.ChatID != 0 || event.Kind != KindOther || event.ChatType != ChatGroup {
		t.Fatalf("неподдерживаемое под фиктивным чатом: %+v", event)
	}
}

func TestNormalizeRejectsUnparsable(t *testing.T) {
	cases := []string{
		`"строка"`,            // не объект
		`{"foo": 1}`,          // нет update_id
		`{"update_id": 1.5}`,  // не целое
		`{"update_id": "12"}`, // не число
	}
	for _, raw := range cases {
		if _, err := NormalizeUpdate(json.RawMessage(raw)); err == nil {
			t.Fatalf("неразборчивый update %s должен давать ошибку", raw)
		}
	}
}
