//go:build live

// Живой смоук модели (Р-критерий этапа 9, без Telegram): приложение целиком —
// poller → буферы → worker → ход → применение → доставка — на настоящем
// провайдере (MODEL_* из .env) и заглушке Bot API, в Telegram ничего не уходит.
// Каждый тест — отдельная чистая база, схема применяется миграциями.
// Запуск (Postgres docker-compose на 5433 — см. 9.1):
//
//	TEST_DATABASE_URL=postgres://dio:dio@localhost:5433/dio?sslmode=disable \
//	  go test -tags "db,live" ./internal/app/ -run TestLive -v
//
// Без MODEL_* в .env тесты пропускаются с понятной причиной. Тег live не
// входит ни в один гейт: живые вызовы вне гейтов (философия №6 — стоимость).
package app

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/dio-agent/internal/background"
	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// newLiveHarness — приложение целиком на живом провайдере: модель из .env,
// Telegram — заглушка, база — отдельная чистая, логи — в stderr (прогон
// наблюдают глазами).
func newLiveHarness(t *testing.T) *harness {
	t.Helper()

	if err := runtime.LoadDotEnv("../../.env"); err != nil {
		t.Fatalf(".env не читается: %v", err)
	}
	endpoint := strings.TrimSpace(os.Getenv("MODEL_ENDPOINT"))
	apiKey := strings.TrimSpace(os.Getenv("MODEL_API_KEY"))
	model := strings.TrimSpace(os.Getenv("MODEL_MODEL"))
	if endpoint == "" || apiKey == "" || model == "" {
		t.Skip("живой смоук пропущен: в .env не заданы MODEL_ENDPOINT / MODEL_API_KEY / MODEL_MODEL")
	}
	retries := 3
	if v, err := strconv.Atoi(os.Getenv("MODEL_MAX_RETRIES")); err == nil && v > 0 {
		retries = v
	}

	dsn := newTestDB(t)
	return newHarnessDSNTuned(t, dsn, func(o *Options) {
		o.Model = character.NewChatConfig(endpoint, apiKey, model, 4000, 90_000, retries)
		o.Logger = log.New(os.Stderr, "live ", log.LstdFlags)
		o.Config.LogLevel = runtime.LogLevelInfo
		// Дневной бюджет §16: смоуку хватает небольшого пула.
		o.Config.Model = &runtime.ModelSettings{DailyCallsLimit: 50}
		// Продакшен-подобный темп: дедлайн ping singleton равен интервалу
		// проверки (singleton.go StartWatch), E2E-настройка 20 мс под живой
		// нагрузкой даёт ложную «потерю соединения»; фон — без частого
		// heartbeat-спама.
		o.SingletonWatch = 2 * time.Second
		o.Background = background.Options{MinInterval: 2 * time.Second, MaxInterval: 2 * time.Second, RetryBackoff: time.Second}
	})
}

// eventuallyLive — ожидание с живым дедлайном: свободные модели отвечают
// десятками секунд, плюс повторы провайдера при сбоях.
func eventuallyLive(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("условие не выполнено за 4 минуты: %s", what)
}

// mustExec — посев/служебный SQL, ошибка фатальна.
func mustExec(h *harness, sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatalf("sql %q: %v", sql, err)
	}
}

// streamEvent — payload первого события заданного типа из журнала.
func streamEvent(h *harness, typ string) (string, bool) {
	return queryStringOK(h, `SELECT payload::text FROM event_streams WHERE type = $1 ORDER BY created_at LIMIT 1`, typ)
}

// replies — доставленные заглушке тексты (что персонаж отправил в чат).
func replies(h *harness) []string {
	var out []string
	for _, c := range h.api.callsOf("sendMessage") {
		out = append(out, c.Text)
	}
	return out
}

// TestLiveDirectTurn — прямой ход целиком на живой модели: строгая схема
// TurnResult на реальном ответе, применение одной транзакцией, доставка.
func TestLiveDirectTurn(t *testing.T) {
	h := newLiveHarness(t)

	h.api.push(1, 100, "private", "Привет! Двумя короткими фразами: кто ты и чем увлекаешься?")

	eventuallyLive(t, "ход применён (turns.status=applied)", func() bool {
		return queryInt(h, `SELECT count(*) FROM turns WHERE status = 'applied'`) >= 1
	})
	eventuallyLive(t, "ответ дошёл до доставки (sendMessage)", func() bool {
		return len(replies(h)) >= 1
	})

	texts := replies(h)
	t.Logf("ОТВЕТ (%d bubble): %q", len(texts), strings.Join(texts, " | "))
	if mo, ok := streamEvent(h, "model_output"); ok {
		t.Logf("MODEL_OUTPUT: %s", mo)
	}
	n := queryInt(h, `SELECT count(*) FROM event_streams WHERE type = 'tool_call'`)
	t.Logf("tool_call: %d (прямой ход принуждает memory_search первым вызовом)", n)
}

// TestLiveForcedMemoryTool — принудительный вызов инструмента памяти на
// прямом обращении (§10.4, философия №8) против живого провайдера: в чистую
// базу посажен факт, вопрос должен пройти через memory_search и использовать
// найденное в ответе.
func TestLiveForcedMemoryTool(t *testing.T) {
	h := newLiveHarness(t)

	// Посев памяти в скоуп чата 100. В живом ходе provenance назначает
	// runtime; для смоука чтения достаточно прямой вставки.
	mustExec(h, `INSERT INTO provenance (kind, ref, scopes) VALUES ('journal_event', 'event:smoke', ARRAY['chat:100'])`)
	mustExec(h, `INSERT INTO entities (kind, canonical_name, scope_id) VALUES ('person', 'Коля', 'chat:100')`)
	mustExec(h, `INSERT INTO entity_aliases (entity_id, alias, scope_id)
	             SELECT id, 'Коля', 'chat:100' FROM entities WHERE canonical_name = 'Коля'`)
	mustExec(h, `INSERT INTO memory_items (kind, text, scope_id, provenance_id, salience)
	             SELECT 'claim', 'Коля коллекционирует механические клавиатуры и предпочитает тактильные свитчи', 'chat:100', id, 0.9
	             FROM provenance WHERE ref = 'event:smoke'`)
	mustExec(h, `INSERT INTO memory_links (memory_id, entity_id, role)
	             SELECT i.id, e.id, 'subject' FROM memory_items i, entities e
	             WHERE i.text LIKE 'Коля коллекционирует%' AND e.canonical_name = 'Коля'`)

	h.api.push(1, 100, "private", "Привет! Проверь свою память: что я коллекционирую?")

	eventuallyLive(t, "ход применён (turns.status=applied)", func() bool {
		return queryInt(h, `SELECT count(*) FROM turns WHERE status = 'applied'`) >= 1
	})

	toolCalls := queryInt(h, `SELECT count(*) FROM event_streams WHERE type = 'tool_call' AND payload::text LIKE '%memory_search%'`)
	if toolCalls == 0 {
		t.Fatalf("forced tool-calling не сработал: модель не вызвала memory_search — в этом и была цель смоука")
	}
	if tr, ok := streamEvent(h, "tool_result"); ok {
		t.Logf("TOOL_RESULT: %s", tr)
	}

	texts := replies(h)
	joined := strings.ToLower(strings.Join(texts, " | "))
	t.Logf("ОТВЕТ (%d bubble): %q", len(texts), strings.Join(texts, " | "))
	if mo, ok := streamEvent(h, "model_output"); ok {
		t.Logf("MODEL_OUTPUT: %s", mo)
	}
	if !strings.Contains(joined, "клавиатур") {
		t.Logf("ВНИМАНИЕ: ответ не упомянул посаженный факт — memory_search вызван, но найденное в ответ не попало (смотреть MODEL_OUTPUT выше)")
	}
}
