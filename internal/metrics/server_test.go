// Тесты HTTP-сервера метрик (этап 31.1–31.2): кэш снимка, отдача /stats и
// /metrics, деградация на кэш при сбое сбора, /healthz. Источник снимка —
// подделка: юнит-тест не требует живой базы.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource — программируемый источник снимка: считает вызовы и отдаёт
// заданную ошибку.
type fakeSource struct {
	mu    sync.Mutex
	calls int
	snap  Snapshot
	err   error
}

func (f *fakeSource) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Snapshot{}, f.err
	}
	return f.snap, nil
}

func (f *fakeSource) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakePinger — проверка живости базы для /healthz.
type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// clock — управляемое время кэша.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		GeneratedAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Decisions: Decisions{
			Answer: DecisionCounts{Hour1: 3, Day1: 9, Day7: 20},
			React:  DecisionCounts{Hour1: 1, Day1: 2, Day7: 4},
			Ignore: DecisionCounts{Hour1: 2, Day1: 7, Day7: 15},
			Defer:  DecisionCounts{Hour1: 0, Day1: 1, Day7: 2},
		},
		Turns: TurnsStats{
			ByStatus:   map[string]int64{"applied": 10, "skipped": 1, "created": 2},
			Created24h: 5,
			Retried24h: 1,
			Skipped24h: []SkipCount{{Phase: "generate", Reason: "модель не настроена", Count: 2}},
		},
		Model: ModelStats{
			Period: "2026-11-03",
			Pools: map[string]ModelPool{
				"conversation": {Calls: 10, Reserved: 12, Tokens: 4000, Limit: 100, Remaining: 88},
				"background":   {Calls: 20, Reserved: 20, Tokens: 9000, Limit: 400, Remaining: 380},
			},
			PrefixCache: PrefixCacheStats{Samples24h: 6, AvgShare: 0.75},
		},
		MemoryTools: MemoryToolsStats{
			ConversationTurns24h: 10,
			TurnsWithTools24h:    3,
			Share:                0.3,
			Reasons:              map[string]int64{"none": 7, "card_incomplete": 3},
		},
		FirstBubble: LatencyStats{Count: 4, P50Ms: 1200, P95Ms: 5000},
		Queue:       QueueStats{TurnsCreated: 2, OldestTurnAgeSeconds: 35, OutboxPending: 1, IntentionsDue: 0},
		Deliveries: Deliveries{
			Total:   DeliveryStatusCounts{Sent: 10, Unknown: 1, Failed: 2, Cancelled: 1, Pending: 3, RetryWait: 1, Sending: 1},
			Last24h: DeliveryStatusCounts{Sent: 4, Unknown: 1},
		},
		Intentions: IntentionsStats{
			ByKind: map[string]IntentionKindCounts{
				"share": {Created24h: 2, Completed24h: 1, Pending: 1},
			},
			ProactiveLimitPerChatPerDay: 2,
			MinGapMinutes:               240,
			ProactiveByChat:             []ProactiveChat{{ChatID: 777, UsedToday: 1, Remaining: 1}},
			ExplorationsToday:           0,
			ExplorationsLimit:           2,
		},
		Character: CharacterStats{
			Mood:    MoodStats{Valence: 0.2, Arousal: 0.5, SocialEnergy: 0.6},
			Boredom: 0.4,
		},
		Stickers:   StickersStats{Collection: 5, Persona: 4, Saved: 1, Sent24h: 2},
		LifeEvents: LifeEventsStats{Last24h: 1},
		Memory:     MemoryStats{Active: 20, WithVectors: 18},
	}
}

func newTestServer(t *testing.T, src Source, pinger Pinger, clk *clock) *Server {
	t.Helper()
	srv, err := New(Config{
		Port:     1, // только для New; Handler теста слушатель не открывает
		CacheTTL: 15 * time.Second,
		Logger:   log.New(&testWriter{t: t}, "", 0),
		Source:   src,
		DB:       pinger,
		Now:      clk.Now,
	})
	if err != nil {
		t.Fatalf("сервер метрик не собран: %v", err)
	}
	return srv
}

// testWriter отправляет вывод логгера в t.Log (без шума в консоли).
type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestStatsJSON — /stats отдаёт читаемый JSON с метриками §20.
func TestStatsJSON(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	srv := newTestServer(t, &fakeSource{snap: sampleSnapshot()}, fakePinger{}, clk)

	rec := get(t, srv.Handler(), "/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats: код %d, тело %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type /stats = %q, хочу application/json", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats не JSON: %v", err)
	}
	decisions := got["decisions"].(map[string]any)
	answer := decisions["answer"].(map[string]any)
	if answer["1h"].(float64) != 3 || answer["24h"].(float64) != 9 || answer["7d"].(float64) != 20 {
		t.Errorf("decisions.answer = %v, хочу 3/9/20", answer)
	}
	queue := got["queue"].(map[string]any)
	if queue["turns_created"].(float64) != 2 || queue["outbox_pending"].(float64) != 1 {
		t.Errorf("queue = %v", queue)
	}
	server := got["server"].(map[string]any)
	if server["scrape_errors"].(float64) != 0 {
		t.Errorf("server.scrape_errors = %v, хочу 0", server["scrape_errors"])
	}
}

// TestSnapshotCache — БД не дёргается на каждый скрейп: в пределах TTL
// снимок отдаётся из кэша, после — собирается снова (10–30 с, этап 31).
func TestSnapshotCache(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{snap: sampleSnapshot()}
	srv := newTestServer(t, src, fakePinger{}, clk)
	h := srv.Handler()

	for i := 0; i < 3; i++ {
		if rec := get(t, h, "/stats"); rec.Code != http.StatusOK {
			t.Fatalf("GET /stats: код %d", rec.Code)
		}
		clk.Advance(5 * time.Second)
	}
	if n := src.CallCount(); n != 1 {
		t.Fatalf("сборов за 3 скрейпа в пределах TTL = %d, хочу 1", n)
	}

	clk.Advance(16 * time.Second)
	if rec := get(t, h, "/stats"); rec.Code != http.StatusOK {
		t.Fatalf("GET /stats после TTL: код %d", rec.Code)
	}
	if n := src.CallCount(); n != 2 {
		t.Fatalf("сборов после истечения TTL = %d, хочу 2", n)
	}
}

// TestStaleOnError — сбой сбора после успешного снимка не превращает скрейп
// в 500: отдаётся последний снимок, ошибка видна в счётчике (наблюдаемость).
func TestStaleOnError(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	src := &fakeSource{snap: sampleSnapshot()}
	srv := newTestServer(t, src, fakePinger{}, clk)
	h := srv.Handler()

	if rec := get(t, h, "/stats"); rec.Code != http.StatusOK {
		t.Fatalf("первый GET /stats: код %d", rec.Code)
	}
	src.mu.Lock()
	src.err = errors.New("база недоступна")
	src.mu.Unlock()
	clk.Advance(20 * time.Second)

	rec := get(t, h, "/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("сбой сбора должен отдавать кэш, код %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/stats не JSON: %v", err)
	}
	if got["decisions"] == nil {
		t.Errorf("кэшированный снимок не отдан: %v", got)
	}
	if n := got["server"].(map[string]any)["scrape_errors"].(float64); n != 1 {
		t.Errorf("server.scrape_errors = %v, хочу 1", n)
	}

	// Совсем без успешного снимка — честный 500, а не пустые нули.
	srv2 := newTestServer(t, &fakeSource{err: errors.New("нет базы")}, fakePinger{}, clk)
	if rec := get(t, srv2.Handler(), "/stats"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("без кэша и с ошибкой: код %d, хочу 500", rec.Code)
	}
}

// TestHealthz — живой процесс отвечает 200, недоступная база — 503.
func TestHealthz(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	ok := newTestServer(t, &fakeSource{snap: sampleSnapshot()}, fakePinger{}, clk)
	rec := get(t, ok.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz живой: код %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz не JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("/healthz status = %v, хочу ok", body["status"])
	}

	bad := newTestServer(t, &fakeSource{snap: sampleSnapshot()}, fakePinger{err: errors.New("нет связи")}, clk)
	if rec := get(t, bad.Handler(), "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz без базы: код %d, хочу 503", rec.Code)
	}
}

// TestOnlyGet — на эндпоинтах допустим только GET.
func TestOnlyGet(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	srv := newTestServer(t, &fakeSource{snap: sampleSnapshot()}, fakePinger{}, clk)
	for _, path := range []string{"/metrics", "/stats", "/healthz"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: код %d, хочу 405", path, rec.Code)
		}
	}
}

// TestUnknownPath — посторонние пути не обслуживаются.
func TestUnknownPath(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	srv := newTestServer(t, &fakeSource{snap: sampleSnapshot()}, fakePinger{}, clk)
	if rec := get(t, srv.Handler(), "/private/prompt"); rec.Code != http.StatusNotFound {
		t.Errorf("неизвестный путь: код %d, хочу 404", rec.Code)
	}
}
