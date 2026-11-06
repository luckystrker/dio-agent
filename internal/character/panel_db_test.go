//go:build db

// БД-тесты среза 1–2 (решения 2026-11-02): system статичен при разном
// состоянии, панель состояния собирается в конце контекста, история несёт
// абсолютные метки/автора и фото-плейсхолдер, а карточка памяти отменяет
// принудительный поиск на целом разговоре.
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"
)

// seedHistoryEvent вставляет реплику прошлого хода (не текущего).
func (f *fixture) seedHistoryEvent(t *testing.T, ctx context.Context, updateID, messageID int64, kind string, text any, at time.Time) {
	t.Helper()
	if _, err := f.runner.Pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id, occurred_at)
		VALUES ($1, $2, 'private', 777, 'Коля', $3, $4,
		        '{"message":{"message_id":1}}'::jsonb, 'processed', $5, $6)`,
		updateID, testChatID, kind, text, messageID, at); err != nil {
		t.Fatalf("историческое событие не вставлено: %v", err)
	}
}

// TestAssemblerSystemStaticPanelDynamic — system байт-в-байт одинаков при
// разном настроении/отношении/составе пачки; вся динамика — в панели, которая
// идёт после истории и содержит текущее время и паузу.
func TestAssemblerSystemStaticPanelDynamic(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	now := time.Date(2026, 11, 2, 17, 2, 0, 0, loc)
	f.runner.Assembler.Timezone = loc
	f.runner.Assembler.Now = func() time.Time { return now }
	f.runner.Assembler.Memory = f.runner.Memory
	f.runner.Assembler.MemoryPrefetch = true
	f.seedHistoryEvent(t, ctx, 9001, 501, "message", "вчерашняя реплика", now.Add(-(2*time.Hour + 14*time.Minute)))

	batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}
	first, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}

	// Меняем настроение и отношения — system обязан остаться тем же.
	val := 0.8
	if _, err := (NewMoodRepo()).ApplyDelta(ctx, f.pool, f.persona.MoodBaseline, MoodDelta{Valence: &val, Reason: "тест"}); err != nil {
		t.Fatalf("настроение не изменено: %v", err)
	}
	entity, err := (memoryEntityResolver{f}).resolve(t, ctx)
	if err != nil {
		t.Fatalf("актор: %v", err)
	}
	aff := 0.5
	if _, err := (NewRelationshipRepo()).ApplyDelta(ctx, f.pool, entity, testScopeID, &aff, nil); err != nil {
		t.Fatalf("отношение не изменено: %v", err)
	}
	otherBatch := []BatchEvent{
		{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "раз"},
		{EventID: 3, ActorID: 888, ActorName: "Аня", Text: "два"},
	}
	second, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, otherBatch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if first.System != second.System {
		t.Fatalf("system меняется вместе с состоянием/составом пачки:\n--- first ---\n%s\n--- second ---\n%s", first.System, second.System)
	}
	if first.Constraints != second.Constraints {
		t.Fatalf("constraints меняются вместе с пачкой:\n%s\n---\n%s", first.Constraints, second.Constraints)
	}
	for _, dyn := range []string{first.MoodLine, first.RelationshipLine, "Коля"} {
		if dyn != "" && strings.Contains(first.System, dyn) {
			t.Fatalf("динамика %q попала в system:\n%s", dyn, first.System)
		}
	}

	// Панель: служебная, время и пауза, собеседник, состояние, отношение.
	for _, want := range []string{
		statePanelOpen,
		"сейчас: пн 02.11 17:02",
		"с прошлого сообщения в чате: 2 ч 14 мин",
		"ты говоришь с: Коля",
		"твоё состояние: " + first.MoodLine,
		"отношение к собеседнику: " + first.RelationshipLine,
		statePanelClose,
	} {
		if !strings.Contains(first.StatePanel, want) {
			t.Fatalf("в панели нет %q:\n%s", want, first.StatePanel)
		}
	}
	// Панель второго хода: два автора — строка о пачке, имени одного нет.
	if !strings.Contains(second.StatePanel, "в пачке несколько участников") {
		t.Fatalf("панель не отметила нескольких участников:\n%s", second.StatePanel)
	}
	if strings.Contains(second.StatePanel, "ты говоришь с") {
		t.Fatalf("панель назвала одного собеседника в групповой пачке:\n%s", second.StatePanel)
	}
}

// memoryEntityResolver — поиск актора теста по каноническому ключу (M7:
// canonical_name = user:<id>, отображаемое имя «Коля» — алиас).
type memoryEntityResolver struct{ f *fixture }

func (r memoryEntityResolver) resolve(t *testing.T, ctx context.Context) (int64, error) {
	t.Helper()
	var id int64
	err := r.f.pool.QueryRow(ctx,
		`SELECT id FROM entities WHERE scope_id = $1 AND canonical_name = 'user:777'`, testScopeID).Scan(&id)
	return id, err
}

// TestAssemblerHistoryStableLabels — история: абсолютные метки, автор,
// фото-плейсхолдер, байт-стабильность между двумя сборками.
func TestAssemblerHistoryStableLabels(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	at := time.Date(2026, 11, 2, 15, 40, 0, 0, loc)
	f.runner.Assembler.Timezone = loc
	f.runner.Assembler.Now = func() time.Time { return at.Add(time.Hour) }
	f.seedHistoryEvent(t, ctx, 9101, 601, "message", "как дела?", at)
	f.seedHistoryEvent(t, ctx, 9102, 602, "photo", nil, at.Add(time.Minute))
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, telegram_message_id, occurred_at)
		VALUES (9103, $1, 'private', 888, NULL, 'message', 'без имени', '{}'::jsonb, 'processed', 603, $2)`,
		testChatID, at.Add(90*time.Second)); err != nil {
		t.Fatalf("событие без имени не вставлено: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO delivery_outbox (turn_id, position, chat_id, payload, status, created_at)
		VALUES (777001, 0, $1, '{"text":"нормально"}'::jsonb, 'sent', $2)`,
		testChatID, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("ответ не вставлен: %v", err)
	}

	batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "и снова привет"}}
	first, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	second, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран повторно: %v", err)
	}
	if first.HistoryTrimmed {
		t.Fatalf("полная история помечена урезанной")
	}
	if len(first.History) != 4 {
		t.Fatalf("история: %d реплик, хочу 4: %+v", len(first.History), first.History)
	}
	if first.History[0].Author != "Коля" || first.History[0].Content != "как дела?" {
		t.Fatalf("user-реплика без автора: %+v", first.History[0])
	}
	if first.History[1].Content != "[фото]" {
		t.Fatalf("фото без подписи выпало/не помечено: %+v", first.History[1])
	}
	if first.History[2].Author != "user:888" {
		t.Fatalf("фолбэк автора user:<id> не сработал: %+v", first.History[2])
	}
	if first.History[3].Role != "assistant" || first.History[3].Author != "" {
		t.Fatalf("assistant-реплика получила автора: %+v", first.History[3])
	}
	// Байт-стабильность: те же данные — тот же рендер (метки абсолютные).
	for i, m := range first.History {
		if got, want := renderHistoryLine(m, loc), renderHistoryLine(second.History[i], loc); got != want {
			t.Fatalf("история нестабильна: реплика[%d] = %q, хочу %q", i, got, want)
		}
	}
	if line := renderHistoryLine(first.History[0], loc); line != "[пн 02.11 15:40] #m601 Коля: как дела?" {
		t.Fatalf("метка/автор неверны: %q", line)
	}
	// Панель: пауза от последней реплики истории (ответ в 15:42, сейчас 16:40).
	if !strings.Contains(first.StatePanel, "с прошлого сообщения в чате: 58 мин") {
		t.Fatalf("пауза не от последней реплики:\n%s", first.StatePanel)
	}

	// Та же история с другим часовым поясом — другая метка (настройка работает).
	vlad, err := time.LoadLocation("Asia/Vladivostok")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	lineVlad := renderHistoryLine(first.History[0], vlad)
	if lineVlad == renderHistoryLine(first.History[0], loc) {
		t.Fatalf("смена пояса не изменила метку: %q", lineVlad)
	}
}

// TestAssemblerHistoryTrimmed — урезание окном сообщений и бюджетом объёма
// сообщается ходу (решение: без этого нельзя решать про инструменты).
func TestAssemblerHistoryTrimmed(t *testing.T) {
	f, ctx := newFixture(t)
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	f.seedHistoryEvent(t, ctx, 9201, 701, "message", "первая", at)
	f.seedHistoryEvent(t, ctx, 9202, 702, "message", "вторая подлиннее", at.Add(time.Minute))
	f.runner.Assembler.Now = func() time.Time { return at.Add(time.Hour) }
	batch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "третья"}}

	// Окно сообщений: 1 при двух в базе — урезано.
	f.runner.Assembler.HistoryMessages = 1
	trimmed, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if !trimmed.HistoryTrimmed || len(trimmed.History) != 1 {
		t.Fatalf("урезание окном не замечено: trimmed=%v history=%+v", trimmed.HistoryTrimmed, trimmed.History)
	}

	// Окно большое, но бюджет объёма мал — урезано FitHistory.
	f.runner.Assembler.HistoryMessages = 1000
	f.runner.Assembler.HistoryMaxChars = 1
	cut, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, 0)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if !cut.HistoryTrimmed {
		t.Fatalf("урезание бюджетом не замечено: %+v", cut.History)
	}
}

// TestTurnNoMemoryToolsNeededWhenContextWhole — целая история + влезшая
// карточка: инструменты памяти не нужны. С 39.2 определения инструментов
// остаются в запросе (стабильный префикс), а модель получает tool_choice=none
// и отвечает одним вызовом. Урезанная история — инструменты нужны (без forced).
func TestTurnNoMemoryToolsNeededWhenContextWhole(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.MemoryPrefetch = true
	f.runner.Assembler.Memory = f.runner.Memory
	f.runner.Assembler.MemoryPrefetch = true
	f.runner.Assembler.Now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }

	turnID := int64(201)
	f.seedTurn(t, ctx, turnID, "привет")
	fake := &toolsSequenceClient{final: `{"decision":"answer","messages":["привет"]}`}
	cfg := NewChatConfigWithClient(fake)
	fake.cfg = cfg
	f.runner.Model = cfg

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID, Direct: true,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if out.LLMCalls != 1 {
		t.Fatalf("модельных вызовов %d, хочу ровно 1: %v", out.LLMCalls, fake.toolCounts())
	}
	counts := fake.toolCounts()
	closed := fake.closedFlags()
	if len(counts) != 1 || counts[0] == 0 {
		t.Fatalf("набор инструментов должен остаться объявленным (39.2): %v", counts)
	}
	if len(closed) != 1 || !closed[0] {
		t.Fatalf("целая история + карточка: инструменты не закрыты tool_choice=none: %v", closed)
	}
}

// TestTurnToolsWhenHistoryTrimmed — урезанная история: инструменты выданы,
// лимит 3, forced-вызова нет (ответ модели сразу финальный).
func TestTurnToolsWhenHistoryTrimmed(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.MemoryPrefetch = true
	f.runner.Assembler.Memory = f.runner.Memory
	f.runner.Assembler.MemoryPrefetch = true
	f.runner.Assembler.HistoryMaxChars = 1 // история из одной прошлой реплики урежется
	f.runner.Assembler.Now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }
	f.seedHistoryEvent(t, ctx, 9301, 801, "message", "прошлая реплика", time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	f.seedHistoryEvent(t, ctx, 9302, 802, "message", "ещё одна прошлая реплика", time.Date(2026, 11, 2, 11, 1, 0, 0, time.UTC))

	turnID := int64(202)
	f.seedTurn(t, ctx, turnID, "новый вопрос")
	final := agotest.NewTestConfig(&events.ModelResponse{Message: &events.ModelMessage{
		Content: `{"decision":"answer","messages":["ответ"]}`,
	}})
	rec := &recordingClient{inner: final}
	f.runner.Model = NewChatConfigWithClient(rec)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID, Direct: true,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "новый вопрос"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if out.LLMCalls != 1 {
		t.Fatalf("модельных вызовов %d, хочу ровно 1", out.LLMCalls)
	}
	rec.mu.Lock()
	sizes := append([]int(nil), rec.sizes...)
	rec.mu.Unlock()
	if len(sizes) != 1 || sizes[0] != 3 {
		t.Fatalf("урезанная история должна дать 3 инструмента памяти: %v", sizes)
	}
}

// TestTurnModelOutputPanelAndHistoryWire — панель стоит в последнем
// пользовательском сообщении перед пачкой, история — до неё с метками.
// Проверяется на реальном проводе BuildMessages через сохранённые события.
func TestTurnModelOutputPanelAndHistoryWire(t *testing.T) {
	f, ctx := newFixture(t)
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("зона: %v", err)
	}
	f.runner.Timezone = loc
	f.runner.Assembler.Timezone = loc
	f.runner.Assembler.Now = func() time.Time { return time.Date(2026, 11, 2, 17, 2, 0, 0, loc) }
	f.seedHistoryEvent(t, ctx, 9401, 901, "message", "вчерашнее", time.Date(2026, 11, 2, 14, 0, 0, 0, loc))

	turnID := int64(203)
	f.seedTurn(t, ctx, turnID, "сегодняшнее")
	fake := &toolsSequenceClient{final: `{"decision":"answer","messages":["ответ"]}`}
	cfg := NewChatConfigWithClient(fake)
	fake.cfg = cfg
	f.runner.Model = cfg

	batch := []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "сегодняшнее"}}
	if _, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID, Direct: true, Batch: batch}); err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	// Собираем контекст заново (детерминированные часы) и проверяем провод.
	bctx, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, batch, turnID)
	if err != nil {
		t.Fatalf("контекст не собран: %v", err)
	}
	if len(bctx.History) != 1 || bctx.History[0].Content != "вчерашнее" {
		t.Fatalf("история на проводе не та: %+v", bctx.History)
	}
	userText := composeUserText(bctx.StatePanel, strings.Join(bctx.Batch, "\n"))
	if !strings.HasPrefix(userText, statePanelOpen) || !strings.HasSuffix(userText, "сегодняшнее") {
		t.Fatalf("панель не первая часть сообщения:\n%s", userText)
	}
	if !strings.Contains(userText, "сейчас: пн 02.11 17:02") {
		t.Fatalf("панель без текущего времени:\n%s", userText)
	}
	// Панель одноразовая: в историю следующих ходов она не попадает.
	for _, m := range bctx.History {
		if strings.Contains(m.Content, statePanelOpen) || strings.Contains(m.Content, "состояние хода") {
			t.Fatalf("панель просочилась в историю: %+v", m)
		}
	}
}

// TestTurnRecordsCachedTokens — кэш префикса провайдера доезжает до budget
// actual хода отдельным полем (наблюдаемость, решение 2026-11-02).
func TestTurnRecordsCachedTokens(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(204)
	f.seedTurn(t, ctx, turnID, "привет")
	prompt, completion, cached := int64(100), int64(20), int64(80)
	cfg := agotest.NewTestConfig(&events.ModelResponse{
		Message: &events.ModelMessage{Content: `{"decision":"ignore"}`},
		Usage:   events.Usage{PromptTokens: &prompt, CompletionTokens: &completion, CacheReadInputTokens: &cached},
	})
	f.runner.Model = NewChatConfigWithClient(cfg)
	if _, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID, Direct: true,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}}); err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	var got int64
	if err := f.pool.QueryRow(ctx,
		`SELECT (actual->>'cached_tokens')::bigint FROM budget_reservations
		  WHERE operation_id = $1`, fmt.Sprintf("model:turn:%d:0:0", turnID)).Scan(&got); err != nil {
		t.Fatalf("cached_tokens не записан в actual: %v", err)
	}
	if got != 80 {
		t.Fatalf("cached_tokens = %d, хочу 80", got)
	}
}

// TestAssemblerPhotoBatchSameMemoryContext — 21.8: фото без подписи не меняет
// сборку панели/карточки и план инструментов. query для карточки пуст (прежний
// отбор), а не плейсхолдер «[фото]»: карточка собирается, инструменты так же
// не нужны, как на текстовом ходе с полной историей.
func TestAssemblerPhotoBatchSameMemoryContext(t *testing.T) {
	f, ctx := newFixture(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	f.runner.Assembler.Memory = f.runner.Memory
	f.runner.Assembler.MemoryPrefetch = true
	f.runner.Assembler.Now = func() time.Time { return now }

	photoRaw := json.RawMessage(`{"update_id":9,"message":{"message_id":9,
		"photo":[{"file_id":"thumb","width":90,"height":90},{"file_id":"big","width":1280,"height":720}]}}`)
	textBatch := []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "привет"}}
	photoBatch := []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Raw: photoRaw}}

	textCtx, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, textBatch, 0)
	if err != nil {
		t.Fatalf("текстовый контекст не собран: %v", err)
	}
	photoCtx, err := f.runner.Assembler.Assemble(ctx, f.pool, f.persona, testScopeID, testChatID, photoBatch, 0)
	if err != nil {
		t.Fatalf("фото-контекст не собран: %v", err)
	}

	if !textCtx.MemoryCardIncluded || !photoCtx.MemoryCardIncluded {
		t.Fatalf("карточка не собрана: текст=%v фото=%v",
			textCtx.MemoryCardIncluded, photoCtx.MemoryCardIncluded)
	}
	if photoCtx.MemoryCardEntries == 0 {
		t.Fatalf("на фото-ходе карточка пуста — откат к прежнему отбору не сработал")
	}
	if !strings.HasPrefix(photoCtx.StatePanel, statePanelOpen) {
		t.Fatalf("панель фото-хода не собрана:\n%s", photoCtx.StatePanel)
	}
	if strings.Contains(photoCtx.StatePanel, "[фото") || strings.Contains(photoCtx.StatePanel, "фото без подписи") {
		t.Fatalf("плейсхолдер фото попал в панель/карточку:\n%s", photoCtx.StatePanel)
	}

	textPlan := planMemoryTools(Input{}, textCtx, true, true, 5)
	photoPlan := planMemoryTools(Input{}, photoCtx, true, true, 5)
	if photoPlan != textPlan {
		t.Fatalf("план инструментов на фото отличается от текстового: фото=%+v текст=%+v",
			photoPlan, textPlan)
	}
	if photoPlan.Enabled {
		t.Fatalf("фото-ход без нужды включил инструменты памяти: %+v", photoPlan)
	}
}
