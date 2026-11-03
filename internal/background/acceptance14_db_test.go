//go:build db

// Этап 14 (дополнительные углы приёмки, дополняют bg_attempts_db_test.go):
// (1) потолок попыток — на ЦИКЛ: пока cooldown стоит, время само по себе цикл
// не перезапускает; сам cooldown — запись в runtime_state под ключом спеки
// со сроком 24 ч; (2) полный круг «сбой×5 → cooldown → срок вышел → успех» —
// консолидация доводит эпизод до summarized_at; (3) потолок намерения: ход
// инициативы в таблице turns — skipped (инвариант 5).
package background

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luckystrker/ago/config"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"

	"github.com/luckystrker/dio-agent/internal/character"
)

// consolidationCooldownSpec — срок cooldown из спеки (CHECKLIST 14.2): 24 ч.
// Литерал, а не констановка кода: тест держит спеку, а не реализацию.
const consolidationCooldownSpec = 24 * time.Hour

// TestConsolidationCapNoFreshAttemptWhileCooled — обратная сторона потолка:
// потолок считается на цикл, и внутри цикла время его не сбрасывает. После
// терминального сбоя попыток cooldown держит эпизод: шестой вызов не делается,
// даже если между тактами прошло много времени (сдвиг часов почти на весь срок
// cooldown, но до истечения).
func TestConsolidationCapNoFreshAttemptWhileCooled(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	base := f.now
	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, base.Add(-5*time.Hour))
	lastID := ids[len(ids)-1] // LastEventID эпизода — последнее событие
	ep := Episode{ChatID: bgChatID, ChatType: "private", EventIDs: ids, LastEventID: lastID}

	for i := 0; i < 7; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}
	if fc.calls != maxGenerateAttempts {
		t.Fatalf("вызовов модели %d, хочу ровно %d (потолок попыток)", fc.calls, maxGenerateAttempts)
	}

	// Спека 14.2: cooldown — запись в runtime_state под ключом
	// bg:consolidate:cooldown:<чат>:<событие>, срок 24 ч от постановки.
	key := fmt.Sprintf("bg:consolidate:cooldown:%d:%d", bgChatID, lastID)
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT value FROM runtime_state WHERE key = $1`, key).Scan(&raw); err != nil {
		t.Fatalf("cooldown не найден в runtime_state по ключу %q (спека 14.2): %v", key, err)
	}
	var st struct {
		Until time.Time `json:"until"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("cooldown %q не разбирается: %v", key, err)
	}
	want := base.Add(consolidationCooldownSpec)
	if d := st.Until.Sub(want); d > time.Minute || d < -time.Minute {
		t.Fatalf("срок cooldown %v, хочу ~%v (24 ч от постановки, спека 14.2)", st.Until, want)
	}

	// Шестой вызов внутри цикла не делается: часы идут большими кусками
	// (суммарно 23 ч 55 мин — «много», но cooldown ещё стоит).
	elapsed := time.Duration(0)
	for i, step := range []time.Duration{12 * time.Hour, 11 * time.Hour, 55 * time.Minute} {
		elapsed += step
		f.setNow(base.Add(elapsed))
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d после сдвига часов не удался: %v", i+1, err)
		}
		if fc.calls != maxGenerateAttempts {
			t.Fatalf("прошло %v (cooldown ещё стоит): вызовов модели %d, хочу %d — внутри цикла попытки не возобновляются",
				elapsed, fc.calls, maxGenerateAttempts)
		}
	}
	if cooled, err := f.sched.consolidationCooled(ctx, ep); err != nil || !cooled {
		t.Fatalf("cooldown должен держаться до истечения срока: cooled=%v err=%v", cooled, err)
	}
}

// TestConsolidationFreshCycleCompletesAfterCooldown — полный круг из спеки:
// вечный сбой×5 → cooldown → срок вышел → консолидация с нормальным провайдером
// доводит эпизод до конца: ровно один новый вызов, события с summarized_at,
// WAL-шаги generate и apply закрыты.
func TestConsolidationFreshCycleCompletesAfterCooldown(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	lastID := ids[len(ids)-1]
	unitID := consolidationUnitID(bgChatID, lastID)

	for i := 0; i < 7; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}
	if fc.calls != maxGenerateAttempts {
		t.Fatalf("вызовов модели %d, хочу ровно %d (потолок попыток)", fc.calls, maxGenerateAttempts)
	}

	// Срок cooldown вышел, провайдер встал: свежий цикл обязан довести эпизод.
	f.setNow(f.now.Add(consolidationCooldownSpec + time.Minute))
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
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

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после cooldown не удался: %v", err)
	}
	if hits != 1 {
		t.Fatalf("здоровый провайдер получил %d вызовов, хочу ровно 1 (свежий цикл — одна попытка на удачный провайдер)", hits)
	}

	// Эпизод доведён до конца: все события помечены summarized_at.
	var unsummarized int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).
		Scan(&unsummarized); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if unsummarized != 0 {
		t.Fatalf("после свежего цикла эпизод должен быть сведён, не помечено событий: %d", unsummarized)
	}

	// WAL-след полного круга: generate и apply закрыты.
	done := walDoneNames(f.walSteps(t, ctx, unitID))
	if !done[stepGenerate] {
		t.Fatal("WAL: generate должен быть закрыт после успешной генерации")
	}
	if !done[stepApply] {
		t.Fatal("WAL: apply должен быть закрыт после применения")
	}
}

// TestIntentionCapSkipsTurnInDB — потолок на инициативном пути: ход инициативы,
// созданный до генерации, остаётся в таблице turns ровно один (повторные
// попытки ход не плодят) и получает статус skipped (инвариант 5); причина
// пропуска — в журнале хода (turn_skipped с причиной).
func TestIntentionCapSkipsTurnInDB(t *testing.T) {
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
		t.Fatalf("вызовов модели %d, хочу ровно %d (потолок попыток)", fc.calls, maxGenerateAttempts)
	}
	if status := f.intentionStatus(t, ctx, id); status != "failed" {
		t.Fatalf("намерение %d: статус %q, хочу failed", id, status)
	}

	// Ход в таблице turns: ровно одна строка, статус skipped (инвариант 5) —
	// recovery такой ход не подберёт, повторов с тем же контекстом не будет.
	rows, err := f.pool.Query(ctx, `SELECT status FROM turns WHERE chat_id = $1`, bgChatID)
	if err != nil {
		t.Fatalf("ходы инициативы не читаются: %v", err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			t.Fatalf("ход не читается: %v", err)
		}
		statuses = append(statuses, st)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ходы не дочитаны: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("ходов инициативы в turns %d, хочу ровно 1 (повторы генерации не плодят ходы)", len(statuses))
	}
	if statuses[0] != "skipped" {
		t.Fatalf("ход инициативы: статус %q, хочу skipped (инвариант 5)", statuses[0])
	}

	// Причина пропуска — в журнале хода: turn_skipped с непустой причиной.
	var turnID int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM turns WHERE chat_id = $1`, bgChatID).Scan(&turnID); err != nil {
		t.Fatalf("ход инициативы не читается: %v", err)
	}
	var reason string
	if err := f.pool.QueryRow(ctx,
		`SELECT payload->>'reason' FROM event_streams WHERE stream_id = $1 AND type = 'turn_skipped'`,
		character.TurnStreamID(turnID)).Scan(&reason); err != nil {
		t.Fatalf("turn_skipped в журнале хода %d не найден (инвариант 5): %v", turnID, err)
	}
	if reason == "" {
		t.Fatal("turn_skipped обязан нести причину (инвариант 5: терминальный пропуск с причиной)")
	}
}

// garbageClient — модельный клиент, всегда отвечающий текстом, который не
// парсится в ConsolidationProposal (стоит на месте невалидного proposal после
// repair, §11.3): считает вызовы.
type garbageClient struct{ calls int }

func (g *garbageClient) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	g.calls++
	return &events.ModelResponse{Message: &events.ModelMessage{
		Content: "Простите, я задумался и забыл вернуть JSON.",
	}}, nil
}

// scriptedProposalClient — скриптовая консолидационная модель: первые garbage
// вызовов отвечают мусором (невалидный proposal), остальные — валидным
// ConsolidationProposal.
type scriptedProposalClient struct {
	garbage int
	calls   int
}

func (c *scriptedProposalClient) Call(context.Context, []events.Event, *agocontext.Context, config.CallOptions) (*events.ModelResponse, error) {
	c.calls++
	if c.calls <= c.garbage {
		return &events.ModelResponse{Message: &events.ModelMessage{
			Content: "Мусор вместо JSON.",
		}}, nil
	}
	return &events.ModelResponse{Message: &events.ModelMessage{Content: consolidationJSON(
		"Эпизод про винил: Коля показал коллекцию.",
		"Собирает винил, гордится коллекцией")}}, nil
}

// TestConsolidationInvalidProposalLeavesGenerateOpen — ветвь «невалидный
// proposal после repair» (терминальный сбой консолидации): одна попытка
// генерации (вызов + repair), cooldown в runtime_state, и ГЛАВНОЕ — WAL-шаг
// generate НЕ помечается исполненным: иначе после срока cooldown возобновление
// спотыкается о «generate отмечен, но proposal в WAL нет» и ошибается каждым
// тактом вечно. Вторая фаза: срок cooldown вышел, модель отвечает валидным
// proposal — эпизод доводится до конца (события summarized_at).
func TestConsolidationInvalidProposalLeavesGenerateOpen(t *testing.T) {
	f, ctx := newFixture(t)
	gc := &garbageClient{}
	f.sched.model = character.NewChatConfigWithClient(gc)

	ids, _ := f.seedEpisode(t, ctx, bgChatID, "private", 3, f.now.Add(-5*time.Hour))
	lastID := ids[len(ids)-1]
	unitID := consolidationUnitID(bgChatID, lastID)

	// Три такта: первый делает единственную попытку (вызов + repair), остальные
	// обязаны молчать — терминальный сбой не повторяется тактами.
	for i := 0; i < 3; i++ {
		if err := f.sched.Tick(ctx); err != nil {
			t.Fatalf("такт %d не удался: %v", i+1, err)
		}
	}
	if gc.calls != 2 {
		t.Fatalf("вызовов модели %d, хочу ровно 2 (одна попытка: вызов + repair, повторов тактами нет)", gc.calls)
	}

	// Cooldown стоит.
	key := fmt.Sprintf("bg:consolidate:cooldown:%d:%d", bgChatID, lastID)
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT value FROM runtime_state WHERE key = $1`, key).Scan(&raw); err != nil {
		t.Fatalf("cooldown не найден в runtime_state по ключу %q (спека 14.2): %v", key, err)
	}
	// WAL generate не помечен исполненным: после cooldown возобновление
	// перезапускает генерацию, а не спотыкается о «proposal в WAL нет».
	if done := walDoneNames(f.walSteps(t, ctx, unitID)); done[stepGenerate] {
		t.Fatal("generate не должен быть помечен исполненным при невалидном proposal: возобновление после cooldown споткнётся о «proposal в WAL нет» и будет ошибаться каждым тактом вечно")
	}

	// Срок cooldown вышел, провайдер встал: эпизод обязан дойти до конца.
	f.setNow(f.now.Add(consolidationCooldownSpec + time.Minute))
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
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

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт после cooldown не удался: %v", err)
	}
	if hits != 1 {
		t.Fatalf("здоровый провайдер получил %d вызовов, хочу ровно 1 — возобновление после cooldown обязано перезапустить генерацию", hits)
	}
	var unsummarized int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, ids).
		Scan(&unsummarized); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if unsummarized != 0 {
		t.Fatalf("после возобновления эпизод должен быть сведён, не помечено событий: %d", unsummarized)
	}
	done := walDoneNames(f.walSteps(t, ctx, unitID))
	if !done[stepGenerate] || !done[stepApply] {
		t.Fatalf("WAL после успешного возобновления: generate=%v apply=%v, хочу оба закрытыми", done[stepGenerate], done[stepApply])
	}
}

// TestExploreAttemptCapNoTurn — потолок попыток на exploration (§15, §16):
// у explore нет хода-строки, после потолка намерение терминально failed,
// в turns для чата НЕ появляется строк (skipTurn не вызывается), вызовов
// дальше нет.
func TestExploreAttemptCapNoTurn(t *testing.T) {
	f, ctx := newFixture(t)
	fc := &failingClient{}
	f.sched.model = character.NewChatConfigWithClient(fc)

	f.seedPrivateChat(t, ctx, "тихо в чате", f.now.Add(-2*time.Hour))
	id := f.createIntention(t, ctx, KindExplore, "виниловые пластинки", -6, bgChatID)

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
		t.Fatalf("вызовов модели %d, хочу ровно %d (потолок попыток)", fc.calls, maxGenerateAttempts)
	}
	if status := f.intentionStatus(t, ctx, id); status != "failed" {
		t.Fatalf("намерение %d: статус %q, хочу failed", id, status)
	}

	// У explore нет хода-строки: в turns для чата пусто.
	var turns int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM turns WHERE chat_id = $1`, bgChatID).Scan(&turns); err != nil {
		t.Fatalf("ходы не читаются: %v", err)
	}
	if turns != 0 {
		t.Fatalf("в turns для чата %d строк, хочу 0 — exploration не создаёт ход и не помечает пропуск хода", turns)
	}
	// Усиленный ассерт (29.4): пропуск несуществующего хода не записан ни в
	// поток «turn:0», ни с turn_id=0 — безусловный skipTurn(0) был бы только
	// шумом журнала, а не ошибкой статуса, и без этой проверки проходил бы.
	var phantom int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM event_streams
		WHERE type = 'turn_skipped'
		  AND (stream_id = 'turn:0' OR payload->>'turn_id' = '0')`).Scan(&phantom); err != nil {
		t.Fatalf("журнал пропусков не читается: %v", err)
	}
	if phantom != 0 {
		t.Fatalf("turn_skipped несуществующего хода записан %d раз — skipTurn вызван без хода", phantom)
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

// TestCooldownOfSiblingEpisodeDoesNotBlock — cooldown чужого эпизода не мешает:
// два готовых эпизода одного чата; первый получает невалидный proposal и уходит
// в cooldown посреди такта, второй обязан свестись успешно В ТОМ ЖЕ такте —
// cooldown-ключ эпизода (chat:last_event) не путает эпизоды.
func TestCooldownOfSiblingEpisodeDoesNotBlock(t *testing.T) {
	f, ctx := newFixture(t)
	sc := &scriptedProposalClient{garbage: 2} // вызовы 1–2 — эпизод A (вызов+repair), вызов 3 — эпизод B
	f.sched.model = character.NewChatConfigWithClient(sc)

	base := f.now
	idsA, _ := f.seedEpisode(t, ctx, bgChatID, "private", 2, base.Add(-9*time.Hour))
	idsB, _ := f.seedEpisode(t, ctx, bgChatID, "private", 2, base.Add(-4*time.Hour))
	lastA := idsA[len(idsA)-1]
	lastB := idsB[len(idsB)-1]

	// Разрез: тишина между эпизодами ~5 ч (граница DM — 4 ч), оба готовы.
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("такт двух эпизодов не удался: %v", err)
	}
	if sc.calls != 3 {
		t.Fatalf("вызовов модели %d, хочу ровно 3 (эпизод A: вызов+repair с мусором, эпизод B: валидный)", sc.calls)
	}

	// Эпизод A: cooldown стоит, события не сведены.
	var raw []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT value FROM runtime_state WHERE key = $1`,
		fmt.Sprintf("bg:consolidate:cooldown:%d:%d", bgChatID, lastA)).Scan(&raw); err != nil {
		t.Fatalf("cooldown эпизода A не найден в runtime_state (спека 14.2): %v", err)
	}
	// Эпизод B: cooldown-ключа нет и события сведены — чужой cooldown его не задел.
	var unsummarizedB int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, idsB).
		Scan(&unsummarizedB); err != nil {
		t.Fatalf("события эпизода B не читаются: %v", err)
	}
	if unsummarizedB != 0 {
		t.Fatalf("эпизод B должен свестись в том же такте, не помечено событий: %d (cooldown чужого эпизода не должен его задерживать)", unsummarizedB)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT value FROM runtime_state WHERE key = $1`,
		fmt.Sprintf("bg:consolidate:cooldown:%d:%d", bgChatID, lastB)).Scan(&raw); err == nil {
		t.Fatal("у эпизода B не должно быть cooldown-записи: cooldown ставится эпизоду, а не чату")
	}

	// Эпизод A остаётся несведённым до истечения своего cooldown.
	var unsummarizedA int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE id = ANY($1) AND summarized_at IS NULL`, idsA).
		Scan(&unsummarizedA); err != nil {
		t.Fatalf("события эпизода A не читаются: %v", err)
	}
	if unsummarizedA != len(idsA) {
		t.Fatalf("эпизод A на cooldown: несведённых событий %d, хочу %d (сводиться не должен)", unsummarizedA, len(idsA))
	}
}
