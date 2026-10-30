// Юнит-тесты политики ошибок отправки §13.2 (чистая функция classifySendError):
// 429 honor retry_after; 5xx — экспоненциальный retry до лимита; 4xx — failed;
// обрыв/таймаут без ответа — unknown (исход неизвестен).
package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClassifySendError(t *testing.T) {
	retryBase := 2 * time.Second
	cases := []struct {
		name     string
		err      error
		attempts int
		want     outcomeKind
		wantWait time.Duration
	}{
		{"429 honor retry_after", &APIError{Method: "sendMessage", HTTPStatus: 429, RetryAfter: 3 * time.Second}, 1, outcomeRetryWait, 3 * time.Second},
		{"429 без retry_after — 1с (паритет TS)", &APIError{Method: "sendMessage", HTTPStatus: 429}, 1, outcomeRetryWait, time.Second},
		{"5xx первая попытка — экспоненциальная пауза", &APIError{Method: "sendMessage", HTTPStatus: 502}, 1, outcomeRetryWait, 2 * time.Second},
		{"5xx вторая попытка — пауза ×2", &APIError{Method: "sendMessage", HTTPStatus: 500}, 2, outcomeRetryWait, 4 * time.Second},
		{"5xx лимит попыток исчерпан — failed", &APIError{Method: "sendMessage", HTTPStatus: 503}, 5, outcomeFailed, 0},
		{"4xx — подтверждённая постоянная ошибка", &APIError{Method: "sendMessage", HTTPStatus: 403}, 1, outcomeFailed, 0},
		{"таймаут без ответа — unknown", context.DeadlineExceeded, 1, outcomeUnknown, 0},
		{"обрыв соединения — unknown", errors.New("connection reset"), 1, outcomeUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySendError(tc.err, tc.attempts, 5, retryBase)
			if got.kind != tc.want {
				t.Fatalf("исход = %d, хочу %d", got.kind, tc.want)
			}
			if got.wait != tc.wantWait {
				t.Fatalf("пауза = %v, хочу %v", got.wait, tc.wantWait)
			}
		})
	}
}

func TestClassifyRetryAfter429BeatsBackoff(t *testing.T) {
	// §13.2: учитывать retry_after — он сильнее собственной экспоненты.
	got := classifySendError(&APIError{HTTPStatus: 429, RetryAfter: 30 * time.Second}, 4, 5, time.Second)
	if got.kind != outcomeRetryWait || got.wait != 30*time.Second {
		t.Fatalf("429: %+v", got)
	}
}
