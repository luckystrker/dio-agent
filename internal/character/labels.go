// Метки сообщений истории (этап 37.2). Метка — короткий детерминированный
// указатель на сообщение ЭТОГО чата: #m<telegram_message_id>. Она не меняется
// между ходами (Telegram id стабилен), поэтому кэш префикса истории не
// страдает, а модель может ответить реплаем на старое сообщение и попросить
// переслать его. Runtime принимает метку только из текущего чата (§9.1).
package character

import (
	"fmt"
	"strconv"
	"strings"
)

// maxMessageLabelLen — потолок метки в TurnResult (мусор не раздувает схему).
const maxMessageLabelLen = 32

// MessageLabel — метка сообщения по Telegram id; 0 — метки нет.
func MessageLabel(telegramMessageID int64) string {
	if telegramMessageID <= 0 {
		return ""
	}
	return fmt.Sprintf("#m%d", telegramMessageID)
}

// ParseMessageLabel разбирает метку терпимо к слабой модели: «#m123», «m123»,
// «#123». Возвращает Telegram message_id; ok=false — это не метка.
func ParseMessageLabel(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)
	if len(s) > 1 && (s[0] == 'm' || s[0] == 'M') {
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
