// Дополнительный тест recover-защиты доставки (пункт 16.2, приёмка): цикл
// DeliveryLoop.Run обязан не только поймать первую панику такта, но и
// продолжать тикать — «печатает…» приём и фоновые ходы не должны умирать из-за
// бага одного тика доставки.
package telegram

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// TestDeliveryRunContinuesAfterTickPanic — 16.2(в): после паники такта (битый
// репозиторий: nil-deref в ClaimDue) цикл продолжает тикать (в логе ≥2 ошибки
// такта — вторая доказывает жизнь после первой) и чисто останавливается
// отменой контекста.
func TestDeliveryRunContinuesAfterTickPanic(t *testing.T) {
	var buf syncBuffer
	loop := NewDeliveryLoop(nil, nil, nil,
		log.New(&buf, "", 0), DeliveryOptions{Interval: 2 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(buf.String(), "ошибка такта") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("цикл доставки должен пережить панику такта и тикать дальше (нужно ≥2 ошибок такта), лог: %q", buf.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery loop не остановился после отмены")
	}
}
