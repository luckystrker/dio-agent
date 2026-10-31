//go:build db

// Приёмка находки 2026-10-31 (продолжение этапа 14): исчерпанный дневной лимит
// пула background — НЕ сбой провайдера. До фикса каждый такт фона отнимал
// попытку у потолка генерации (cp.Attempt++), и на пятом такте консолидация
// уезжала в cooldown 24 ч — ровно накануне обнуления пула. Фон замирал на
// сутки, хотя бюджет оживал через часы.
//
// Два пути: (1) лимит виден заранее — задача откладывается, не трогая ни
// попытку, ни WAL; (2) гонка с соседней задачей — модельный вызов отказал
// ErrBudgetExhausted, попытка возвращается.
package background

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// budgetExhaustedClient — модельный клиент, который отвечает «дневной лимит
// пула исчерпан»: так middleware бюджета сообщает об исчерпании, когда резерв
// не прошёл уже после peek-проверки (гонка с соседней фоновой задачей).
type budgetExhaustedClient struct{ calls int }

func (c *budgetExhaustedClient) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	c.calls++
	return nil, fmt.Errorf("провайдер: %w", runtime.ErrBudgetExhausted)
}

// checkpointAttempt — номер попытки генерации из чекпоинта задачи (0 — задачи
// ещё нет: попытки не расходовались).
func (f *fixture) checkpointAttempt(t *testing.T, ctx context.Context, unitID string) int {
	t.Helper()
	var attempt int
	err := f.pool.QueryRow(ctx,
		`SELECT COALESCE((state->>'attempt')::int, 0) FROM task_checkpoints WHERE task_id = $1`,
		unitID).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("чекпоинт задачи %q не читается: %v", unitID, err)
	}
	return attempt
}

// TestConsolidationDefersOnBudgetExhausted — лимит пула виден заранее: такты
// идут, но консолидация откладывается, не расходуя попытку, не ставя cooldown
// и не вызывая модель. Сколько тактов ни прошло бы, эпизод не «проваливается».
func TestConsolidationDefersOnBudgetExhausted(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	// Пул фона выжат: лимит 1, единственный вызов уже зарезервирован чужой
	// операцией (как в жизни — другой фоновой задачей).
	f.sched.dailyLimit = 1
	ok, err := f.budget.Reserve(ctx, "model:bg:foreign:1", 1,
		runtime.BudgetPoolBackground, 1, f.sched.dailyLimit)
	if err != nil || !ok {
		t.Fatalf("подготовка пула не удалась: ok=%v err=%v", ok, err)
	}

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	lastID := ids[len(ids)-1]
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: lastID}
	unitID := consolidationUnitID(bgChatID, lastID)

	// Тактов больше, чем потолок попыток: без фикса тут уже стоял бы cooldown.
	for i := 0; i < maxGenerateAttempts+3; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}

	if fc.calls != 0 {
		t.Fatalf("модель вызвана %d раз при исчерпанном пуле, хочу 0", fc.calls)
	}
	if attempt := f.checkpointAttempt(t, ctx, unitID); attempt != 0 {
		t.Fatalf("попыток генерации израсходовано %d, хочу 0: исчерпанный день — не сбой генерации", attempt)
	}
	cooled, cerr := f.sched.consolidationCooled(ctx, ep)
	if cerr != nil {
		t.Fatalf("cooldown не читается: %v", cerr)
	}
	if cooled {
		t.Fatal("cooldown поставлен на исчерпанном дневном лимите: фон замрёт на сутки после обнуления пула")
	}
	if done := walDoneNames(f.walSteps(t, ctx, unitID)); done[stepGenerate] {
		t.Fatal("generate не должен быть помечен исполненным при отложенной консолидации")
	}

	// Бюджет обнулился (новый день) — консолидация идёт сразу, без разгона.
	f.sched.dailyLimit = 1000
	f.setNow(f.now.Add(2 * time.Minute))
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после обнуления пула не удался: %v", err)
	}
	if fc.calls == 0 {
		t.Fatal("после обнуления пула модель не вызвана: консолидация не возобновилась")
	}
}

// TestConsolidationBudgetRaceKeepsAttempt — гонка: peek прошёл, а модельный
// вызов отказал исчерпанным лимитом. Попытка возвращается, cooldown не ставится.
func TestConsolidationBudgetRaceKeepsAttempt(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &budgetExhaustedClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	lastID := ids[len(ids)-1]
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: lastID}
	unitID := consolidationUnitID(bgChatID, lastID)

	const tacts = 3
	for i := 0; i < tacts; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}

	if fc.calls != tacts {
		t.Fatalf("вызовов модели %d, хочу %d (каждый такт — одна попытка)", fc.calls, tacts)
	}
	if attempt := f.checkpointAttempt(t, ctx, unitID); attempt != 0 {
		t.Fatalf("после исчерпанного бюджета попыток %d, хочу 0 (попытка возвращается)", attempt)
	}
	cooled, cerr := f.sched.consolidationCooled(ctx, ep)
	if cerr != nil {
		t.Fatalf("cooldown не читается: %v", cerr)
	}
	if cooled {
		t.Fatal("cooldown поставлен на исчерпанном дневном лимите")
	}
}

// TestIntentionDefersOnBudgetExhausted — тот же класс на инициативном пути:
// намерение возвращается на завтра, попытка не расходуется, модель не зовётся.
func TestIntentionDefersOnBudgetExhausted(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	f.sched.dailyLimit = 1
	ok, err := f.budget.Reserve(ctx, "model:bg:foreign:1", 1,
		runtime.BudgetPoolBackground, 1, f.sched.dailyLimit)
	if err != nil || !ok {
		t.Fatalf("подготовка пула не удалась: ok=%v err=%v", ok, err)
	}

	f.seedPrivateChat(t, ctx, "тихо в чате", f.now.Add(-2*time.Hour))
	id := f.createIntention(t, ctx, KindCheckIn, "как дела", -6, bgChatID)

	for i := 0; i < maxGenerateAttempts+2; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}

	if fc.calls != 0 {
		t.Fatalf("модель вызвана %d раз при исчерпанном пуле, хочу 0", fc.calls)
	}
	if attempt := f.checkpointAttempt(t, ctx, intentionUnitID(id)); attempt != 0 {
		t.Fatalf("попыток генерации намерения %d, хочу 0", attempt)
	}
	if status := f.intentionStatus(t, ctx, id); status == "failed" {
		t.Fatal("намерение терминально провалено на исчерпанном дневном лимите")
	}
	// Отложено на завтра, а не на RetryBackoff.
	if nb := f.intentionNotBefore(t, ctx, id); !nb.After(f.now.Add(6 * time.Hour)) {
		t.Fatalf("намерение не отложено на завтра: not_before=%s", nb)
	}
}
