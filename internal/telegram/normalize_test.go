// Юнит-тесты нормализации update (§6.1, шаг 1). Поведение —
// src/telegram/normalize.ts (+ PBT-инварианты normalize.pbt.test.ts этапа 2
// как табличные примеры).
package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestNormalizeReplyQuotesTarget(t *testing.T) {
	// Реплай в группе: модель обязана видеть, на что отвечают, даже когда
	// исходное сообщение вне окна недавнего разговора (находка приёмки Р).
	event := mustNormalize(t, `{
		"update_id": 10,
		"message": {
			"message_id": 11,
			"from": {"id": 42, "first_name": "Коля"},
			"chat": {"id": -5, "type": "group"},
			"reply_to_message": {
				"message_id": 9,
				"from": {"id": 7, "first_name": "Яна"},
				"text": "а поехали в субботу на море"
			},
			"text": "я за"
		}
	}`)
	if event.Kind != KindReply {
		t.Fatalf("kind = %s, хочу reply", event.Kind)
	}
	if event.Text == nil || *event.Text != "[в ответ на Яна: «а поехали в субботу на море»] я за" {
		t.Fatalf("цитата не в тексте события: %+v", event.Text)
	}
}

func TestNormalizeReplyQuoteFallbacks(t *testing.T) {
	// Цитируемое без автора.
	noName := mustNormalize(t, `{"update_id":1,"message":{"message_id":2,"chat":{"id":1,"type":"private"},
		"reply_to_message":{"message_id":1,"text":"тишина"},"text":"ок"}}`)
	if noName.Text == nil || *noName.Text != "[в ответ на «тишина»] ок" {
		t.Fatalf("цитата без автора: %+v", noName.Text)
	}
	// Цитируемое фото: подписи нет.
	photo := mustNormalize(t, `{"update_id":2,"message":{"message_id":3,"chat":{"id":1,"type":"private"},
		"reply_to_message":{"message_id":2,"photo":[{"file_id":"t","width":10,"height":10}]},"text":"это что?"}}`)
	if photo.Text == nil || *photo.Text != "[в ответ на (фото)] это что?" {
		t.Fatalf("цитата фото: %+v", photo.Text)
	}
	// Цитируемое без всего: пометка вместо пустоты.
	empty := mustNormalize(t, `{"update_id":3,"message":{"message_id":4,"chat":{"id":1,"type":"private"},
		"reply_to_message":{"message_id":3},"text":"?"}}`)
	if empty.Text == nil || *empty.Text != "[в ответ на (без текста)] ?" {
		t.Fatalf("цитата без текста: %+v", empty.Text)
	}
	// Реплай без собственного текста: событие не превращается в «без текста».
	onlyQuote := mustNormalize(t, `{"update_id":4,"message":{"message_id":5,"chat":{"id":1,"type":"private"},
		"reply_to_message":{"message_id":4,"from":{"id":7,"first_name":"Яна"},"text":"ты тут?"}}}`)
	if onlyQuote.Text == nil || *onlyQuote.Text != "[в ответ на Яна: «ты тут?»] " {
		t.Fatalf("только цитата: %+v", onlyQuote.Text)
	}
}

func TestNormalizeReplyQuoteCapped(t *testing.T) {
	long := strings.Repeat("ы", 500)
	event := mustNormalize(t, fmt.Sprintf(`{"update_id":1,"message":{"message_id":2,"chat":{"id":1,"type":"private"},
		"reply_to_message":{"message_id":1,"from":{"id":7,"first_name":"Яна"},"text":%q}},"text":"ага"}`, long))
	if event.Text == nil {
		t.Fatal("текст события пуст")
	}
	if got := []rune(*event.Text); len(got) > 200+50 {
		t.Fatalf("цитата не обрезана: %d рун", len(got))
	}
	if !strings.Contains(*event.Text, "…") {
		t.Fatalf("обрезанная цитата без многоточия: %q", *event.Text)
	}
}

func TestNormalizeSticker(t *testing.T) {
	// Стикер — не «сообщение без текста»: модель видит эмодзи (находка приёмки Р).
	event := mustNormalize(t, `{
		"update_id": 20,
		"message": {
			"message_id": 21,
			"from": {"id": 42, "first_name": "Коля"},
			"chat": {"id": 42, "type": "private"},
			"sticker": {"emoji": "😂", "is_video": false, "set_name": "UtyaDuck"}
		}
	}`)
	if !event.Supported || event.ChatID != 42 {
		t.Fatalf("стикер должен поддерживаться в своём чате: %+v", event)
	}
	if event.Kind != KindSticker {
		t.Fatalf("kind = %s, хочу sticker", event.Kind)
	}
	if event.Text == nil || *event.Text != "[стикер 😂]" {
		t.Fatalf("эмодзи стикера не в тексте события: %+v", event.Text)
	}
	// Стикер без эмодзи (бывает у наборов): остаётся стикером для пачки,
	// а не «сообщением без текста».
	bare := mustNormalize(t, `{"update_id":22,"message":{"message_id":23,"chat":{"id":42,"type":"private"},
		"sticker":{"is_animated":true}}}`)
	if !bare.Supported || bare.Text == nil || *bare.Text != "[стикер]" {
		t.Fatalf("стикер без эмодзи: %+v", bare)
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
