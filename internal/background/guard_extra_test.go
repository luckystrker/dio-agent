// Дополнительные юнит-тесты recover-обёрток задач (пункт 16.2, приёмка):
// обёртки исполнения намерения и консолидации обязаны превращать панику в
// ошибку ЭТОЙ задачи (лог ERROR) и оставаться пригодными к следующему вызову —
// тестировщик дополняет юнит-проверку обёртки такта (guard_test.go) до всех
// трёх поясов изоляции.
package background

import (
	"context"
	"log"
	"strings"
	"testing"
)

// TestRunIntentionGuardedConvertsPanic — 16.2(б): паника исполнения намерения
// возвращается ошибкой задачи и пишется в лог ERROR; обёртка переживает
// панику и зовётся снова (такт продолжается другими задачами).
func TestRunIntentionGuardedConvertsPanic(t *testing.T) {
	var buf syncBuffer
	s := &Scheduler{
		logger: log.New(&buf, "", 0),
		opts:   Options{}.withDefaults(),
		// now nil: runIntention падает на первой строке (started := s.now()).
	}
	for i := 0; i < 2; i++ {
		out, err := s.runIntentionGuarded(context.Background(), Intention{ID: 42, Kind: KindExplore})
		if err == nil || !strings.Contains(err.Error(), "паника") {
			t.Fatalf("вызов %d: паника должна возвращаться ошибкой задачи: out=%+v err=%v", i, out, err)
		}
	}
	logged := buf.String()
	if !strings.Contains(logged, "ERROR") || !strings.Contains(logged, "паника исполнения намерения 42") {
		t.Fatalf("паника задачи должна писаться в лог ERROR, лог: %q", logged)
	}
}

// TestRunConsolidationGuardedConvertsPanic — 16.2(б) для консолидации: паника
// эпизода — ошибка задачи, не такта: ERROR в лог, обёртка готова к следующему
// вызову.
func TestRunConsolidationGuardedConvertsPanic(t *testing.T) {
	var buf syncBuffer
	s := &Scheduler{
		logger: log.New(&buf, "", 0),
		opts:   Options{}.withDefaults(),
		// now nil: runConsolidation падает на первой строке.
	}
	for i := 0; i < 2; i++ {
		out, err := s.runConsolidationGuarded(context.Background(), Episode{ChatID: 7})
		if err == nil || !strings.Contains(err.Error(), "паника") {
			t.Fatalf("вызов %d: паника должна возвращаться ошибкой задачи: out=%+v err=%v", i, out, err)
		}
	}
	logged := buf.String()
	if !strings.Contains(logged, "ERROR") || !strings.Contains(logged, "паника консолидации эпизода чата 7") {
		t.Fatalf("паника задачи должна писаться в лог ERROR, лог: %q", logged)
	}
}
