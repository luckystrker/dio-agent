//go:build db

// Этап 14 (находка VPS 2026-10-31): фон выжигал дневной пул на вечном сбое
// провайдера. Тесты: (1) консолидация обязана нести заголовок сессии
// x-opencode-session, как прямой ход и инициатива/explore; (2) потолок попыток
// генерации — вечный сбой не может повторяться каждым тактом бесконечно.
package background

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

// failingClient — модельный клиент, всегда падающий (стоит на месте
// транзитного сбоя провайдера): считает вызовы.
type failingClient struct{ calls int }

func (f *failingClient) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	f.calls++
	return nil, errors.New("провайдер недоступен (тестовый вечный сбой)")
}

// TestConsolidationCarriesSessionHeader — заголовок x-opencode-session доходит
// до провайдера на консолидации (находка 2026-10-31: HTTP 400 MissingSessionID
// — runConsolidationModel клонировал конфиг без session_id, тогда как прямой
// ход и инициатива/explore заголовок ставили).
func TestConsolidationCarriesSessionHeader(t *testing.T) {
	f, ctx := newFixture(t)

	var gotSession, gotUA, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		resp := map[string]any{
			"id": "x", "model": "test",
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message": map[string]any{
					"content": consolidationJSON(
						"Эпизод про псыж: Коля принёс редкую банку.",
						"Уважает коллекционные газировки"),
				},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	f.sched.model = character.NewChatConfig(server.URL, "k", "test", 1000, 0, 0)

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт консолидации не удался: %v", err)
	}

	if gotPath == "" || !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("консолидация не дошла до провайдера: path=%q", gotPath)
	}
	// Ассерт ниже сравнивает две строки: без этой проверки он «проходил» на
	// двух пустых (гард сессии не сработал — пусто и на проводе, и в ожидании).
	if gotSession == "" {
		t.Fatal("x-opencode-session пуст: заголовок не долетел до провайдера, сравнение с ожиданием было бы вакуумным (14.1)")
	}
	if gotSession != sessionForChat(bgChatID) {
		t.Fatalf("x-opencode-session = %q, хочу %q", gotSession, sessionForChat(bgChatID))
	}
	if !strings.HasPrefix(gotUA, "dio-agent/") {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	// Задача реально завершилась: события эпизода помечены.
	var unsummarized int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).
		Scan(&unsummarized); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if unsummarized != 0 {
		t.Fatalf("эпизод должен быть сведён, не помечено событий: %d", unsummarized)
	}
}

// TestConsolidationAttemptCap — потолок попыток генерации: вечный транзитный
// сбой повторяется не бесконечно, а maxGenerateAttempts раз, затем — cooldown
// и терминальный сбой задачи; после срока cooldown — свежий цикл попыток
// (находка 2026-10-31: 1000 вызовов модели в пустоту за день).
func TestConsolidationAttemptCap(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	lastID := ids[len(ids)-1] // LastEventID эпизода — последнее событие, не id записи памяти
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: lastID}
	unitID := consolidationUnitID(bgChatID, lastID)

	for i := 0; i < 7; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}
	if fc.calls != maxGenerateAttempts {
		t.Fatalf("вызовов модели %d, хочу ровно %d (потолок попыток)", fc.calls, maxGenerateAttempts)
	}

	// Cooldown записан — детекция больше не предлагает эпизод.
	cooled, err := f.sched.consolidationCooled(ctx, ep)
	if err != nil || !cooled {
		t.Fatalf("cooldown не стоит: cooled=%v err=%v", cooled, err)
	}
	// WAL generate не помечен исполненным: возобновление после cooldown
	// перезапускает генерацию, а не спотыкается о «proposal в WAL нет».
	if done := walDoneNames(f.walSteps(t, ctx, unitID)); done[stepGenerate] {
		t.Fatal("generate не должен быть помечен исполненным при терминальном сбое попыток")
	}

	// После срока cooldown — свежий цикл: ровно одна новая попытка.
	f.setNow(f.now.Add(consolidationCooldown + time.Minute))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после cooldown не удался: %v", err)
	}
	if fc.calls != maxGenerateAttempts+1 {
		t.Fatalf("после cooldown вызовов %d, хочу %d (свежий цикл попыток)", fc.calls, maxGenerateAttempts+1)
	}
}

// TestIntentionAttemptCap — потолок попыток на инициативном пути: транзитный
// вечный сбой не повторяется поверх RetryBackoff бесконечно, намерение
// терминально проваливается (находка 2026-10-31, класс дефекта консолидации).
func TestIntentionAttemptCap(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	f.seedPrivateChat(t, ctx, "тихо в чате", f.now.Add(-2*time.Hour))
	id := f.createIntention(t, ctx, KindCheckIn, "как дела", -6, bgChatID)

	// Повтор намерения гейтится not_before против часов БД (ClaimDue): паузу
	// повтора прижимаем к минимуму и ждём реального времени между тактами.
	f.sched.opts.RetryBackoff = 2 * time.Millisecond
	for i := 0; i < 7; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fc.calls != maxGenerateAttempts {
		t.Fatalf("вызовов модели %d, хочу ровно %d", fc.calls, maxGenerateAttempts)
	}
	if status := f.intentionStatus(t, ctx, id); status != "failed" {
		t.Fatalf("намерение %d: статус %q, хочу failed", id, status)
	}

	// После потолка — тишина: ни модели, ни повторов.
	time.Sleep(5 * time.Millisecond)
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после потолка не удался: %v", err)
	}
	if fc.calls != maxGenerateAttempts {
		t.Fatalf("после потолка вызовов %d, хочу %d", fc.calls, maxGenerateAttempts)
	}
}
