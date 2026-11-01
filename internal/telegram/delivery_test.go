// Юнит-тесты политики ошибок отправки §13.2 (чистая функция classifySendError):
// 429 honor retry_after; 5xx — экспоненциальный retry до лимита; 4xx — failed;
// обрыв/таймаут без ответа — unknown (исход неизвестен).
package telegram

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
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

// syncBuffer — потокобезопасный буфер лога (delivery loop пишет из своей
// горутины, тест читает из своей).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestDeliveryRunSurvivesTickPanic — 16.2: паника такта доставки (битый
// репозиторий: nil- deref в ClaimDue) пишется в лог и не роняет цикл и
// процесс; раньше паника в горутине Run убивала весь бинарник.
func TestDeliveryRunSurvivesTickPanic(t *testing.T) {
	var buf syncBuffer
	loop := NewDeliveryLoop(nil, nil, nil,
		log.New(&buf, "", 0), DeliveryOptions{Interval: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(buf.String(), "паника") {
		if time.Now().After(deadline) {
			t.Fatalf("паника такта не залогирована, лог: %q", buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery loop не остановился после отмены")
	}
	if !strings.Contains(buf.String(), "ошибка такта") {
		t.Fatalf("паника такта должна проходить обычным путём ошибки такта, лог: %q", buf.String())
	}
}
