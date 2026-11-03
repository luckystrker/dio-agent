package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestTransientModelError — M10: транзитный отказ провайдера отличается от
// постоянного 4xx и отмены хода: у первого длинный повтор, у второго — нет.
func TestTransientModelError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"5xx", errors.New(`config: chat completions returned 502: upstream error`), true},
		{"429", errors.New(`config: chat completions returned 429: rate limit`), true},
		{"4xx постоянная", errors.New(`config: chat completions returned 400: MissingSessionID`), false},
		{"401 постоянная", errors.New(`config: chat completions returned 401: unauthorized`), false},
		{"сеть", &net.DNSError{Err: "no such host", Name: "api"}, true},
		{"отмена хода", context.Canceled, false},
		{"обычная ошибка", errors.New("ход не удался"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientModelError(tc.err); got != tc.want {
				t.Fatalf("transientModelError(%v) = %v, хочу %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWorkerTransientBackoffAndLimit — M10: транзитный сбой не упирается в
// трёхпопыточный потолок за 6 секунд и ждёт заметно дольше базовой экспоненты.
func TestWorkerTransientBackoffAndLimit(t *testing.T) {
	w := New(Deps{}, Options{
		RetryBase: 2 * time.Second, RetryMax: 5 * time.Minute,
		MaxAttempts: 3, TransientMaxAttempts: 8,
	})
	transient := fmt.Errorf("generation: %w", errors.New(`config: chat completions returned 503: unavailable`))
	permanent := errors.New(`config: chat completions returned 400: bad request`)

	if got := w.attemptLimitFor(transient); got != 8 {
		t.Fatalf("лимит для транзитного = %d, хочу 8", got)
	}
	if got := w.attemptLimitFor(permanent); got != 3 {
		t.Fatalf("лимит для постоянного = %d, хочу 3", got)
	}
	if got := w.pauseFor(1, transient); got != transientMinPause {
		t.Fatalf("пауза транзитного = %v, хочу не меньше %v", got, transientMinPause)
	}
	if got := w.pauseFor(1, permanent); got != 2*time.Second {
		t.Fatalf("пауза постоянного = %v, хочу 2s", got)
	}
	if got := w.pauseFor(6, transient); got != 64*time.Second {
		t.Fatalf("пауза транзитного на 6-й попытке = %v, хочу 64s", got)
	}
}
