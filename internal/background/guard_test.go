// Юнит-тесты recover-защиты фона (пункт 16.2). Паника такта планировщика и
// паника одной фоновой задачи не должны ронять процесс: планировщик крутится
// в собственной горутине, где без recover умирает весь бинарник — вместе с
// приёмом и доставкой.
package background

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
)

// syncBuffer — потокобезопасный буфер лога (Run и его такты пишут из своей
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

// TestTickGuardedRecoversPanic — обёртка такта ловит панику и пишет её в лог
// ERROR (16.2а): nil-пул — первый же запрос Tick паникует; обёртка должна
// вернуть управление, а следующий такт — выполниться (цикл не умирает).
func TestTickGuardedRecoversPanic(t *testing.T) {
	var buf syncBuffer
	s := &Scheduler{
		logger: log.New(&buf, "", 0),
		opts:   Options{}.withDefaults(),
		// pool и прочие зависимости nil: Tick падает на первом же запросе.
	}
	s.tickGuarded(context.Background()) // паника ловится, тест жив
	s.tickGuarded(context.Background()) // цикл продолжается следующим тактом
	logged := buf.String()
	if !strings.Contains(logged, "ERROR") || !strings.Contains(logged, "паника") {
		t.Fatalf("паника такта должна писаться в лог ERROR, лог: %q", logged)
	}
}
