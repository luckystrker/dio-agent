//go:build db && eval && live

// Живой прогон поведенческого набора (§21.2: «оценка проводится на
// сохранённых диалоговых сценариях с несколькими прогонами: ответы
// стохастичны») на реальной модели из .env. Telegram — заглушка, в чат ничего
// не уходит. Запуск (модель и БД — как у живого смоука):
//
//	MODEL_MODEL=… TEST_DATABASE_URL=… \
//	  go test -tags "db,eval,live" ./internal/app/ -run TestEvalSuiteLive -v -timeout 20m
//
// Кейс без прохода — это наблюдение о поведении модели, а не падение сборки:
// отчёт пишется в evals/results/live.json, тест фейлится только если живой
// pass_rate ниже разумного floor'а (задан EVAL_MIN_PASS_RATE, по умолчанию 0.5).
package app

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/evalsuite"
)

func TestEvalSuiteLive(t *testing.T) {
	cases := evalSuite(t)
	live := func(t *testing.T, c evalsuite.Case) *evalRun {
		t.Helper()
		h := newLiveHarness(t) // скриптовые ответы кейса игнорируются: решает живая модель
		scope := c.Setup.Scope
		if scope == "" {
			scope = "chat:100"
		}
		var chatID int64
		if _, err := fmt.Sscanf(scope, "chat:%d", &chatID); err != nil {
			t.Fatalf("скоуп кейса не chat:<id>: %q", scope)
		}
		return &evalRun{h: h, t: t, chatID: chatID, scope: scope, wait: 4 * time.Minute}
	}
	rate := runEvalCases(t, "live", cases, live)
	min := 0.5
	if v, err := strconv.ParseFloat(os.Getenv("EVAL_MIN_PASS_RATE"), 64); err == nil && v > 0 {
		min = v
	}
	if rate < min {
		t.Fatalf("живой pass_rate %.2f ниже порога %.2f — поведение модели не дотягивает до эталонов (отчёт: evals/results/live.json)", rate, min)
	}
}
