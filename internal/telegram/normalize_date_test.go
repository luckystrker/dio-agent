// Юнит-проверка 18.1 (без БД): нормализация выносит дату сообщения из всех
// поддерживаемых видов update; даты нет — 0 (occurred_at тогда берёт now()
// приёма на уровне ingest).
package telegram

import (
	"fmt"
	"testing"
)

// TestNormalizeUpdateDate — Date = message.date (секунды Unix) для message,
// edited_message и channel_post; отсутствие даты и неподдерживаемый update — 0.
func TestNormalizeUpdateDate(t *testing.T) {
	const sentAt = int64(1730000000)
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "message",
			raw: fmt.Sprintf(`{"update_id":1,"message":{"message_id":1,"date":%d,`+
				`"text":"привет","from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"}}}`, sentAt),
			want: sentAt,
		},
		{
			name: "edited_message",
			raw: fmt.Sprintf(`{"update_id":2,"edited_message":{"message_id":2,"date":%d,`+
				`"text":"правка","from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"}}}`, sentAt),
			want: sentAt,
		},
		{
			name: "channel_post",
			raw: fmt.Sprintf(`{"update_id":3,"channel_post":{"message_id":3,"date":%d,`+
				`"text":"пост","chat":{"id":-100,"type":"channel"}}}`, sentAt),
			want: sentAt,
		},
		{
			name: "message без date",
			raw: `{"update_id":4,"message":{"message_id":4,"text":"без даты",` +
				`"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"}}}`,
			want: 0,
		},
		{
			name: "неподдерживаемый update",
			raw:  `{"update_id":5,"callback_query":{"id":"cb","from":{"id":42}}}`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := NormalizeUpdate([]byte(tc.raw))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if ev.Date != tc.want {
				t.Fatalf("Date = %d, хочу %d", ev.Date, tc.want)
			}
		})
	}
}
