//go:build db

// Приёмочные проверки 16.4 (независимый тестировщик, свежий контекст):
// фатальность 404 (ТЗ: 401/404, тестировался только 401), нефатальность
// прочих ошибок API (пункт «в»), сброс нарастания паузы 409 после успешного
// getUpdates, ядовитый update в КОНЦЕ пачки (offset в БД догоняет только
// следующим успешным ingest) и транзиентный сбой БД — повтор пачки без
// скипа (главный риск классификации: перепутать транзиент с «неисправимыми
// данными» = молчаливая потеря события).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer — буфер лога, читаемый из теста пока poller пишет.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAccept164FatalOn404 — 404 от getUpdates фатален наравне с 401 (16.4а):
// протухший/чужой токен — честный останов с ошибкой, а не вечные повторы.
func TestAccept164FatalOn404(t *testing.T) {
	pool := newTestPool(t)
	client, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
	})
	poller := NewTelegramPoller(pool, client, nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- poller.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("404: Run вернул nil — тихая глухота вместо ошибки")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 404 {
			t.Fatalf("ошибка должна нести APIError 404: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("404 должен останавливать сразу, а не после пауз: %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("404: Run не вернул ошибку за 5 с")
	}
}

// TestAccept164ServerError500IsNotFatal — прочие ошибки API (пункт «в»):
// 500 дважды, затем здоровый ответ — Run не фатален, пачка доезжает.
func TestAccept164ServerError500IsNotFatal(t *testing.T) {
	pool := newTestPool(t)
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`boom`)) // не-JSON тело → APIError с кодом ответа
			return
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": []json.RawMessage{
			json.RawMessage(rawMessageOf(4000, 42, "после пятисот")),
		}})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM social_events WHERE telegram_update_id = 4000`).Scan(&count); err != nil {
			t.Fatalf("событие не читается: %v", err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("после двух 500 пачка не доехала за 5 с — повтор не работает")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("500 не фатален — Run вернул ошибку: %v", err)
	}
}

// TestAccept164ConflictBackoffResetsAfterSuccess — нарастание паузы 409
// считается ПОДРЯД идущими конфликтами: после успешного getUpdates счётчик
// сбрасывается, следующий 409 снова начинает с базовой паузы.
func TestAccept164ConflictBackoffResetsAfterSuccess(t *testing.T) {
	pool := newTestPool(t)
	var mu sync.Mutex
	var calls []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := len(calls)
		calls = append(calls, time.Now())
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		// вызовы 0,1 → 409; 2 → успех (пустая пачка); 3,4,5 → 409; дальше успех.
		if i == 0 || i == 1 || (i >= 3 && i <= 5) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict"}`))
			return
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": []any{}})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	const base = 120 * time.Millisecond // пауза первого конфликта
	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil, nil,
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: base})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := len(calls)
		mu.Unlock()
		if got >= 6 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("не дошло до 6 вызовов getUpdates — polling стоит")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("409 не фатален — Run вернул ошибку: %v", err)
	}

	// Пауза ошибки живёт МЕЖДУ вызовами: успех возвращает управление сразу
	// (без сна), поэтому g2 — интервал «успех → следующий вызов» ≈ 0.
	// Ожидание: g0≈120, g1≈240 (подряд), g2≈0 (успех), g3≈120 (сброс), g4≈240.
	mu.Lock()
	defer mu.Unlock()
	gap := func(i int) time.Duration { return calls[i].Sub(calls[i-1]) }
	g1, g3, g4 := gap(2), gap(4), gap(5)
	if g1 < 180*time.Millisecond {
		t.Fatalf("второй подряд 409 не удвоил паузу: g0=%v g1=%v", gap(1), g1)
	}
	if g3 > 200*time.Millisecond {
		t.Fatalf("после успешного getUpdates пауза 409 не сбросилась (g1=%v g3=%v — без сброса было бы ~4×базы)", g1, g3)
	}
	if g4 < 180*time.Millisecond {
		t.Fatalf("после сброса нарастание не началось заново: g3=%v g4=%v", g3, g4)
	}
}

// TestAccept164PoisonLastUpdateKeepsReceiving — ядовитый update в КОНЦЕ пачки:
// после скипа offset в БД не догнан (GREATEST пишет только успешный ingest),
// приём обязан продолжиться следующей пачкой и догнать offset там.
func TestAccept164PoisonLastUpdateKeepsReceiving(t *testing.T) {
	pool := newTestPool(t)
	updates := []json.RawMessage{
		json.RawMessage(rawMessageOf(3000, 42, "до яда в конце")),
		json.RawMessage(`{"update_id":3001,"message":{"message_id":2,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"хвост с \u0000"}}`),
		json.RawMessage(rawMessageOf(3005, 42, "новая пачка после яда")),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(raw, &body)
		out := []json.RawMessage{}
		for _, u := range updates {
			var id struct {
				UpdateID int64 `json:"update_id"`
			}
			_ = json.Unmarshal(u, &id)
			if id.UpdateID >= body.Offset {
				out = append(out, u)
			}
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	logs := &lockedBuffer{}
	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil,
		log.New(logs, "", 0),
		PollerOptions{PollTimeout: 100 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx) }()

	// Годные события обеих пачек сохранены, offset в БД догнал конец второй.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var good int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM social_events WHERE telegram_update_id IN (3000, 3005)`).Scan(&good); err != nil {
			t.Fatalf("события не читаются: %v", err)
		}
		offset, _, err := ReadOffset(context.Background(), pool)
		if err != nil {
			t.Fatalf("offset не читается: %v", err)
		}
		if good == 2 && offset == 3006 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("яд в конце пачки заглушил приём: годных %d/2, offset %d (хочу 2 и 3006)", good, offset)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("poller остановился с ошибкой: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("сохранено %d событий (err=%v), хочу 2 — яд не должен попадать в журнал", count, err)
	}
	if out := logs.String(); !bytes.Contains([]byte(out), []byte("ERROR")) || !bytes.Contains([]byte(out), []byte("3001")) {
		t.Fatalf("ERROR-лог с идентификатором ядовитого update не найден: %q", out)
	}
}

// TestAccept164PoisonThenTransientRetriesPastPoison — скип яда продвигает
// offset именно В ПАМЯТИ (§6.1 после 16.4): если за ядом в той же пачке
// идёт транзиентный сбой БД, повтор пачки обязан начинаться уже ПОСЛЕ яда —
// иначе каждая попытка снова упиралась бы в неисправимый update, и приём
// зависал на нём навсегда (ровно то, от чего 16.4 лечит).
func TestAccept164PoisonThenTransientRetriesPastPoison(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Триггер-рубильник: пока взведён, INSERT события 7002 падает
	// транзиентным 57P01 (класс 57 — не ошибки данных, повтор лечит).
	if _, err := pool.Exec(ctx, `
CREATE TABLE poison_kill_switch (k int);
CREATE FUNCTION poison_transient() RETURNS trigger AS $fn$
BEGIN
  IF NEW.telegram_update_id = 7002 AND EXISTS (SELECT 1 FROM poison_kill_switch) THEN
    RAISE EXCEPTION 'transient after poison' USING ERRCODE = '57P01';
  END IF;
  RETURN NEW;
END $fn$ LANGUAGE plpgsql;
CREATE TRIGGER poison_transient BEFORE INSERT ON social_events
  FOR EACH ROW EXECUTE FUNCTION poison_transient();`); err != nil {
		t.Fatalf("триггер не создан: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO poison_kill_switch VALUES (1)`); err != nil {
		t.Fatalf("рубильник не взведён: %v", err)
	}

	// Пачка: 7000 годный, 7001 яд (NUL в text), 7002 годный, но до снятия
	// рубильника — транзиентный сбой. Сервер логирует запрошенные offset.
	updates := []json.RawMessage{
		json.RawMessage(rawMessageOf(7000, 42, "до яда")),
		json.RawMessage(`{"update_id":7001,"message":{"message_id":2,"from":{"id":42,"first_name":"Коля"},"chat":{"id":42,"type":"private"},"text":"яд \u0000"}}`),
		json.RawMessage(rawMessageOf(7002, 42, "за ядом, транзиент")),
	}
	var mu sync.Mutex
	offsetsSeen := map[int64]int{}
	snapshotOffsets := func() map[int64]int {
		mu.Lock()
		defer mu.Unlock()
		cp := make(map[int64]int, len(offsetsSeen))
		for k, v := range offsetsSeen {
			cp[k] = v
		}
		return cp
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		offsetsSeen[body.Offset]++
		mu.Unlock()
		out := []json.RawMessage{}
		for _, u := range updates {
			var id struct {
				UpdateID int64 `json:"update_id"`
			}
			_ = json.Unmarshal(u, &id)
			if id.UpdateID >= body.Offset {
				out = append(out, u)
			}
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": out})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	logs := &lockedBuffer{}
	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil,
		log.New(logs, "", 0),
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 10 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(runCtx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: условие не наступило за 5 с (offsets=%v, журнал: %q)", what, snapshotOffsets(), logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Фаза 1: транзиент на 7002 гоняет повторы — ждём минимум два retry.
	waitFor("повторы пачки с offset 7002", func() bool {
		return snapshotOffsets()[7002] >= 2
	})

	// КЛЮЧЕВАЯ проверка: ни один retry не пришёл с offset ≤ 7001 — скип яда
	// продвинул offset в памяти, пачка не начинает снова упираться в яд.
	// (Стартовый запрос offset 0 — единственное исключение, его не трогаем.)
	seen := snapshotOffsets()
	for _, off := range []int64{7000, 7001} {
		if seen[off] > 0 {
			t.Fatalf("retry пришёл с offset %d — скип яда не продвинул offset в памяти, каждая попытка снова упирается в яд: %v", off, seen)
		}
	}
	// И ERROR-скип яда случился ровно один раз, а не с каждым повтором.
	if n := strings.Count(logs.String(), "7001"); n != 1 {
		t.Fatalf("яд 7001 фигурирует в журнале %d раз (хочу 1 — скип однократный): %q", n, logs.String())
	}

	// Фаза 2: снимаем рубильник — 7002 доезжает, offset БД догоняет до 7003.
	if _, err := pool.Exec(ctx, `DELETE FROM poison_kill_switch`); err != nil {
		t.Fatalf("рубильник не снят: %v", err)
	}
	waitFor("событие 7002 сохранено", func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events WHERE telegram_update_id = 7002`).Scan(&n); err != nil {
			return false
		}
		return n == 1
	})
	waitFor("offset БД 7003", func() bool {
		off, _, err := ReadOffset(ctx, pool)
		return err == nil && off == 7003
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("poller остановился с ошибкой: %v", err)
	}

	// Яд в журнал не попал, оба годных — на месте.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("сохранено %d событий (err=%v), хочу 2 — яд не должен попасть в журнал", count, err)
	}
}

// TestAccept164TransientDBFailureRetriesBatch — транзиентная ошибка БД
// (класс 57, query-уровень: триггер на INSERT поднимает ERRCODE 57P01,
// пока тест его не снимет): пачка повторяется, событие НЕ скипается и не
// теряется; после «заживления» ingest доезжает, offset растёт. Это главный
// риск классификации 16.4: перепутать транзиент с неисправимыми данными —
// молчаливая потеря события с продвижением offset.
func TestAccept164TransientDBFailureRetriesBatch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Триггер-рубильник: пока в kill_switch есть строка, INSERT события 6001
	// падает транзиентным кодом 57P01 (admin shutdown — класс 57, не 22/23).
	if _, err := pool.Exec(ctx, `
CREATE TABLE kill_switch (k int);
CREATE FUNCTION transient_bomb() RETURNS trigger AS $fn$
BEGIN
  IF NEW.telegram_update_id = 6001 AND EXISTS (SELECT 1 FROM kill_switch) THEN
    RAISE EXCEPTION 'transient bomb' USING ERRCODE = '57P01';
  END IF;
  RETURN NEW;
END $fn$ LANGUAGE plpgsql;
CREATE TRIGGER transient_bomb BEFORE INSERT ON social_events
  FOR EACH ROW EXECUTE FUNCTION transient_bomb();`); err != nil {
		t.Fatalf("триггер не создан: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO kill_switch VALUES (1)`); err != nil {
		t.Fatalf("рубильник не взведён: %v", err)
	}

	// Сервер getUpdates: offset 0 → событие 6000, offset 6001 → событие 6001
	// (пока не принято), дальше пусто.
	var mu sync.Mutex
	offsetsSeen := map[int64]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		offsetsSeen[body.Offset]++
		mu.Unlock()
		var batch []json.RawMessage
		if body.Offset <= 6000 {
			batch = []json.RawMessage{json.RawMessage(rawMessageOf(6000, 42, "до сбоя"))}
		} else if body.Offset == 6001 {
			batch = []json.RawMessage{json.RawMessage(rawMessageOf(6001, 42, "после сбоя"))}
		}
		resp, _ := json.Marshal(map[string]any{"ok": true, "result": batch})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(server.Close)

	logs := &lockedBuffer{}
	poller := NewTelegramPoller(pool, NewClient("T", server.URL, server.Client()), nil,
		log.New(logs, "", 0),
		PollerOptions{PollTimeout: 50 * time.Millisecond, ErrorBackoff: 20 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(runCtx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: условие не наступило за 5 с (журнал: %q)", what, logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	rowCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM social_events`).Scan(&n); err != nil {
			t.Fatalf("события не читаются: %v", err)
		}
		return n
	}

	// Фаза 1: событие 6000 сохранено (триггер его не трогает).
	waitFor("первое событие", func() bool { return rowCount() >= 1 })

	// Фаза 2: событие 6001 падает транзиентной ошибкой — пачка ПОВТОРЯЕТСЯ.
	waitFor("несколько повторов пачки", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return offsetsSeen[6001] >= 2
	})
	if n := rowCount(); n != 1 {
		t.Fatalf("во время сбоя сохранено %d событий, хочу 1", n)
	}
	var offset int64
	if err := pool.QueryRow(ctx, `SELECT (value#>>'{}')::bigint FROM runtime_state WHERE key = $1`, PollOffsetKey).Scan(&offset); err != nil || offset != 6001 {
		t.Fatalf("offset при сбое = %d (err=%v), хочу 6001 — транзиентная ошибка не имеет права двигать его", offset, err)
	}
	if out := logs.String(); bytes.Contains([]byte(out), []byte("неисправим")) {
		t.Fatalf("транзиентный сбой БД классифицирован как неисправимые данные — скип события: %q", out)
	}

	// Фаза 3: снимаем рубильник — повтор доезжает, offset растёт.
	if _, err := pool.Exec(ctx, `DELETE FROM kill_switch`); err != nil {
		t.Fatalf("рубильник не снят: %v", err)
	}
	waitFor("второе событие после заживления", func() bool { return rowCount() >= 2 })
	waitFor("offset 6002", func() bool {
		var off int64
		if err := pool.QueryRow(ctx, `SELECT (value#>>'{}')::bigint FROM runtime_state WHERE key = $1`, PollOffsetKey).Scan(&off); err != nil {
			return false
		}
		return off == 6002
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("транзиентный сбой БД не фатален — Run вернул ошибку: %v", err)
	}
}
