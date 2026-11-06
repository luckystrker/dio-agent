// Юнит-тесты классификации «писать больше некуда» (этап 38.7): 403 бота,
// заблокировавшего пользователя, деактивированный аккаунт и несуществующий
// чат — пометка; прочие ошибки (в том числе 403 в группе) — обычный путь §13.2.
package telegram

import "testing"

func TestChatBlockedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"бот заблокирован", &APIError{Method: "sendMessage", HTTPStatus: 403, Description: "Forbidden: bot was blocked by the user"}, true},
		{"аккаунт удалён", &APIError{Method: "sendMessage", HTTPStatus: 403, Description: "Forbidden: user is deactivated"}, true},
		{"чат не найден", &APIError{Method: "sendMessage", HTTPStatus: 400, Description: "Bad Request: chat not found"}, true},
		{"бота выгнали из группы", &APIError{Method: "sendMessage", HTTPStatus: 403, Description: "Forbidden: bot was kicked from the supergroup chat"}, true},
		{"нет прав в группе", &APIError{Method: "sendMessage", HTTPStatus: 403, Description: "Forbidden: not enough rights to send text messages to the chat"}, false},
		{"флуд-контроль", &APIError{Method: "sendMessage", HTTPStatus: 429, Description: "Too Many Requests: retry after 5"}, false},
		{"ошибка не API", errString("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChatBlockedError(tc.err); got != tc.want {
				t.Fatalf("ChatBlockedError = %v, хочу %v", got, tc.want)
			}
		})
	}
}

// errString — простая ошибка для таблицы.
type errString string

func (e errString) Error() string { return string(e) }
