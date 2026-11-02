//go:build db

// БД-тесты хода (этап 9.4, §11–12): полный цикл с событиями журнала,
// идемпотентность применения, невалидный TurnResult не применяется,
// восстановление после сбоя между model_output и дельтами без новой
// генерации, терминальный пропуск, бюджет §16.
package character

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/events"
	agotest "github.com/luckystrker/ago/testing"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

const testChatID = int64(-100500)
const testScopeID = "chat:-100500"

// fixture — готовый ход: схема, персона, контекст-сборщик, журнал, память.
type fixture struct {
	pool    *runtime.Pool
	journal *TurnJournal
	budget  *runtime.BudgetStore
	runner  *Runner
	persona Persona
}

// newFixture собирает окружение хода на чистой тестовой базе.
func newFixture(t *testing.T) (*fixture, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	// Персона: активная запись версии 1.
	persona, err := (NewPersonaRepo()).Active(ctx, pool)
	if err != nil {
		t.Fatalf("персона не создана: %v", err)
	}

	// Память: один внутри-чатовский факт, который инструмент найдёт.
	scope := memory.ChatScope(fmt.Sprintf("%d", testChatID))
	provenanceID, err := (memory.SourceRepo{}).EnsureProvenance(ctx, pool,
		memory.SourceJournalEvent, "seed:chat", []string{scope})
	if err != nil {
		t.Fatalf("provenance не создан: %v", err)
	}
	_, err = (memory.Items{}).Insert(ctx, pool, memory.NewItem{
		Kind:         "claim",
		Text:         "Пользователь любит старые пластинки",
		Salience:     0.7,
		ProvenanceID: provenanceID,
	}, scope)
	if err != nil {
		t.Fatalf("seed-запись не вставлена: %v", err)
	}

	toolset, err := memory.NewToolset(pool, &memory.Searcher{Embedder: nil, Items: memory.Items{}})
	if err != nil {
		t.Fatalf("toolset памяти не создан: %v", err)
	}

	journal := NewTurnJournal(runtime.NewPgStreamStorage(ctx, pool), nil)
	budget := runtime.NewBudgetStore(pool)

	assembler := &Assembler{
		Mood:            NewMoodRepo(),
		Relationships:   NewRelationshipRepo(),
		PromptTemplate:  "",
		HistoryMessages: 1000,
		HistoryMaxChars: 200000,
	}

	f := &fixture{
		pool:    pool,
		journal: journal,
		budget:  budget,
		persona: persona,
	}
	f.runner = &Runner{
		Pool:            pool,
		Journal:         journal,
		Budget:          budget,
		DailyCallsLimit: 0,
		ToolCallsLimit:  5,
		Assembler:       assembler,
		Persona:         persona,
		Memory:          toolset,
	}
	return f, ctx
}

// seedTurn создаёт ход с пачкой одного события (сообщение с actor и message_id).
func (f *fixture) seedTurn(t *testing.T, ctx context.Context, turnID int64, text string) {
	t.Helper()
	_, err := f.runner.Pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id, telegram_message_id)
		VALUES ($1, $2, 'private', 777, 'Коля', 'message', $3,
		        $4::jsonb, 'buffered', $5, 4242)`,
		1_000_000+turnID, testChatID, text,
		fmt.Sprintf(`{"message":{"message_id":%d,"text":%q,"from":{"id":777}}}`, 4242+turnID, text), turnID)
	if err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	if _, err := f.runner.Pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'created')`, turnID, testChatID); err != nil {
		t.Fatalf("turn не вставлен: %v", err)
	}
}

func (f *fixture) withModel(resp ...*events.ModelResponse) *agotest.TestConfig {
	cfg := agotest.NewTestConfig(resp...)
	f.runner.Model = NewChatConfigWithClient(cfg)
	return cfg
}

// eventTypes — типы событий потока хода по порядку.
func (f *fixture) eventTypes(t *testing.T, ctx context.Context, turnID int64) []string {
	t.Helper()
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал хода не читается: %v", err)
	}
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.EventType())
	}
	return out
}

func validFinalJSON() string {
	return `{"decision":"answer","messages":["Да, помню — пластинки это святое."],` +
		`"memory":[{"kind":"preference","text":"Коля коллекционирует пластинки","salience":0.6}],` +
		`"mood_delta":{"valence":0.5,"reason":"приятная тема"},` +
		`"relationship_delta":{"affinity":0.2,"trust":0.1,"reason":"разделённое увлечение"},` +
		`"reason":"отвечаю на воспоминание"}`
}

// TestTurnFullFlow — полный ход: события §12.1 по порядку, применение дельт,
// outbox, однократность при повторном прогоне.
func TestTurnFullFlow(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(101)
	f.seedTurn(t, ctx, turnID, "Помнишь, я рассказывал про пластинки?")

	script := f.withModel(
		&events.ModelResponse{ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
			{ID: "call-1", Name: memory.ToolSearch, Arguments: `{"query":"пластинки"}`},
		}}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: validFinalJSON()}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Помнишь, я рассказывал про пластинки?"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if out.Skipped || out.AlreadyApplied || !out.Apply.Applied {
		t.Fatalf("ход не применился: %+v", out)
	}

	// --- события хода §12.1: полный набор и порядок ключевых типов ---
	types := f.eventTypes(t, ctx, turnID)
	want := []string{EventTypeTurnStarted, EventTypeToolCall, EventTypeToolResult,
		EventTypeModelOutput, EventTypeDeltasApplied, EventTypeDeliveryQueued}
	if len(types) != len(want) {
		t.Fatalf("события хода: %v, хочу %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("порядок событий %v, хочу %v", types, want)
		}
	}

	// --- turn применён, события обработаны ---
	var status string
	if err := f.runner.Pool.QueryRow(ctx, `SELECT status FROM turns WHERE id = $1`, turnID).Scan(&status); err != nil {
		t.Fatalf("turn не читается: %v", err)
	}
	if status != "applied" {
		t.Fatalf("turn status = %q, хочу applied", status)
	}
	var processed int
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT count(*) FROM social_events WHERE turn_id = $1 AND status = 'processed'`, turnID).Scan(&processed); err != nil {
		t.Fatalf("события не читаются: %v", err)
	}
	if processed != 1 {
		t.Fatalf("обработанных событий %d, хочу 1", processed)
	}

	// --- outbox: одно сообщение в очереди доставки (§13.1) ---
	var (
		outboxCount int
		text        string
		outboxID    int64
	)
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outboxCount); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("строк outbox %d, хочу 1", outboxCount)
	}
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT payload->>'text', id FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&text, &outboxID); err != nil {
		t.Fatalf("payload outbox не читается: %v", err)
	}
	if text != "Да, помню — пластинки это святое." {
		t.Fatalf("текст доставки неверен: %q", text)
	}

	// --- mood применён с затуханием и сжатием: delta 0.5 → +0.3 от baseline ---
	var valence float64
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT valence FROM character_state WHERE id = 1`).Scan(&valence); err != nil {
		t.Fatalf("mood не читается: %v", err)
	}
	if valence != f.persona.MoodBaseline.Valence+MaxMoodDeltaPerTurn {
		t.Fatalf("valence = %v, хочу baseline+0.3 = %v", valence, f.persona.MoodBaseline.Valence+MaxMoodDeltaPerTurn)
	}

	// --- отношения: familiarity + лимит affinity/trust (§7.3) ---
	var fam, aff, trust float64
	if err := f.runner.Pool.QueryRow(ctx, `
		SELECT familiarity, affinity, trust FROM relationships
		WHERE person_entity_id = (SELECT id FROM entities WHERE canonical_name = 'Коля') AND scope_id = $1`,
		testScopeID).Scan(&fam, &aff, &trust); err != nil {
		t.Fatalf("отношения не читаются: %v", err)
	}
	if fam != RelationshipLimits.FamiliarityPerSignificantTurn {
		t.Fatalf("familiarity = %v, хочу %v", fam, RelationshipLimits.FamiliarityPerSignificantTurn)
	}
	if aff != RelationshipLimits.AffinityPerTurn || trust != 0.2+RelationshipLimits.TrustPerTurn {
		t.Fatalf("affinity/trust = %v/%v, хочу сжатые лимиты 0.1 и 0.2+0.1", aff, trust)
	}

	// --- аудит-след: событие deltas_applied заявляет СЖАТУЮ дельту (§7.3).
	// Модель просила affinity 0.2 — применено и записано 0.1; журнал —
	// заменитель state_notes и не имеет права заявлять больше.
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	var applied *DeltasApplied
	for _, ev := range evs {
		if d, ok := events.AsValue(ev).(DeltasApplied); ok {
			applied = &d
		}
	}
	if applied == nil || len(applied.Relationships) != 1 {
		t.Fatalf("событие deltas_applied без дельты отношений: %+v", applied)
	}
	ar := applied.Relationships[0]
	if ar.Affinity == nil || *ar.Affinity != RelationshipLimits.AffinityPerTurn {
		t.Fatalf("в событии affinity = %v, хочу сжатые 0.1 (модель просила 0.2)", ar.Affinity)
	}
	if ar.Trust == nil || *ar.Trust != RelationshipLimits.TrustPerTurn {
		t.Fatalf("в событии trust = %v, хочу 0.1 (как применено)", ar.Trust)
	}

	// --- память: кандидат вставлен, источник и разрешение назначены runtime (§10.1) ---
	var memText, srcRef string
	if err := f.runner.Pool.QueryRow(ctx, `
		SELECT m.text, s.ref FROM memory_items m
		JOIN memory_sources ms ON ms.memory_id = m.id
		JOIN sources s ON s.id = ms.source_id
		WHERE m.text LIKE '%коллекционирует%'`).Scan(&memText, &srcRef); err != nil {
		t.Fatalf("кандидат памяти не найден: %v", err)
	}
	if srcRef != fmt.Sprintf("turn:%d", turnID) {
		t.Fatalf("источник кандидата %q, хочу turn:%d", srcRef, turnID)
	}

	// --- повторный прогон: ничего не дублируется (инвариант 4, §12.1) ---
	out2, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "повтор"}}})
	if err != nil {
		t.Fatalf("повторный прогон упал: %v", err)
	}
	if !out2.AlreadyApplied {
		t.Fatalf("повторный прогон должен быть честным no-op: %+v", out2)
	}
	types2 := f.eventTypes(t, ctx, turnID)
	if len(types2) != len(types) {
		t.Fatalf("повторный прогон добавил события: было %d, стало %d", len(types), len(types2))
	}
	var outboxAfter int
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outboxAfter); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outboxAfter != 1 {
		t.Fatalf("повторный прогон создал дубли доставки: %d", outboxAfter)
	}

	// скрипт модели: второй прогон не сделал ни одного вызова
	if calls := len(script.Calls()); calls != 2 {
		t.Fatalf("модель вызвана %d раз (хочу 2 — только первый прогон)", calls)
	}
}

// TestTurnInvalidResultRepaired — невалидный ответ не применяется; один
// repair-retry валидирует ход (§11.3).
func TestTurnInvalidResultRepaired(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(102)
	f.seedTurn(t, ctx, turnID, "Как дела?")

	f.withModel(
		// первый ответ нарушает §11.2: answer без сообщений
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":[]}`}},
		// repair-попытка валидна
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["норм, а у тебя?"]}`}},
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Как дела?"}}})
	if err != nil {
		t.Fatalf("ход с repair упал: %v", err)
	}
	if out.Skipped || !out.Apply.Applied {
		t.Fatalf("repair не спас ход: %+v", out)
	}

	// невалидный вариант НЕ применён: в outbox только текст repair-попытки
	var text string
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "норм, а у тебя?" {
		t.Fatalf("применился не repair-ответ: %q", text)
	}
	// журнал: model_output один, repairs=1
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	modelOutputs := 0
	for _, ev := range evs {
		if mo, ok := events.AsValue(ev).(ModelOutput); ok {
			modelOutputs++
			if mo.Repairs != 1 {
				t.Fatalf("repairs = %d, хочу 1", mo.Repairs)
			}
		}
	}
	if modelOutputs != 1 {
		t.Fatalf("model_output в журнале %d раз, хочу 1", modelOutputs)
	}
}

// TestTurnInvalidResultTerminalSkip — оба ответа невалидны: терминальный
// пропуск (инвариант 5), результат не применяется, повтор не возрождает ход.
func TestTurnInvalidResultTerminalSkip(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(103)
	f.seedTurn(t, ctx, turnID, "Кто ты?")

	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":[]}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"react"}`}}, // react без реакции — тоже невалидно
	)

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Кто ты?"}}})
	if err != nil {
		t.Fatalf("ход не должен падать ошибкой при терминальном пропуске: %v", err)
	}
	if !out.Skipped || out.SkipReason == "" {
		t.Fatalf("ход не помечен пропуском: %+v", out)
	}

	// ничего не применено: outbox пуст, turn остался created
	var outbox, applied int
	if err := f.runner.Pool.QueryRow(ctx, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("невалидный результат применился к доставке: %d строк", outbox)
	}
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT count(*) FROM turns WHERE id = $1 AND status = 'applied'`, turnID).Scan(&applied); err != nil {
		t.Fatalf("turn не читается: %v", err)
	}
	if applied != 0 {
		t.Fatalf("невалидный результат применил turn")
	}

	// повтор: ход остаётся пропущенным, без новых модельных вызовов
	out2, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID})
	if err != nil {
		t.Fatalf("повтор пропущенного хода упал: %v", err)
	}
	if !out2.Skipped {
		t.Fatalf("повтор не увидел терминальный пропуск: %+v", out2)
	}
	types := f.eventTypes(t, ctx, turnID)
	for _, typ := range types {
		if typ == EventTypeDeltasApplied || typ == EventTypeDeliveryQueued {
			t.Fatalf("пропущенный ход применил дельты: %v", types)
		}
	}
}

// TestTurnRecoveryWithoutNewGeneration — сбой между model_output и дельтами:
// сохранённый результат применяется без новой генерации (§12.2, 9.7).
func TestTurnRecoveryWithoutNewGeneration(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(104)
	f.seedTurn(t, ctx, turnID, "Что любишь?")

	// «Упавший процесс»: модель отработала, model_output записан, применение
	// не дошло. Воспроизводим журналом напрямую.
	valid, err := ParseTurnResult([]byte(validFinalJSON()))
	if err != nil {
		t.Fatalf("эталонный результат не валиден: %v", err)
	}
	if err := f.journal.WriteTurnStarted(ctx, &TurnStarted{
		TurnID: turnID, ChatID: testChatID, Kind: "conversation", BatchCount: 1, Model: "test",
	}); err != nil {
		t.Fatalf("turn_started не записан: %v", err)
	}
	if err := f.journal.WriteModelOutput(ctx, turnID, valid, nil, 0); err != nil {
		t.Fatalf("model_output не записан: %v", err)
	}

	// Скрипт ПУСТОЙ: любой вызов модели упал бы — восстановление не должно
	// дойти до модели.
	f.withModel()

	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "Что любишь?"}}})
	if err != nil {
		t.Fatalf("восстановление упало: %v", err)
	}
	if !out.Recovered || !out.Apply.Applied {
		t.Fatalf("восстановление не применило сохранённый результат: %+v", out)
	}

	var text string
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT payload->>'text' FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&text); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if text != "Да, помню — пластинки это святое." {
		t.Fatalf("применился не сохранённый результат: %q", text)
	}
	// model_output в журнале один — генерация не повторялась.
	modelOutputs := 0
	for _, typ := range f.eventTypes(t, ctx, turnID) {
		if typ == EventTypeModelOutput {
			modelOutputs++
		}
	}
	if modelOutputs != 1 {
		t.Fatalf("model_output %d раз, хочу 1 (без новой генерации)", modelOutputs)
	}
}

// TestTurnRecoveryInvalidModelOutputSkips — 18.3: восстановление ревалидирует
// сохранённый model_output. Запись, повреждённая или написанная по старым
// правилам (defer без defer_hours), не должна ронять процесс nil-deref на
// применении: ход получает терминальный skip с ERROR-логом, процесс жив.
func TestTurnRecoveryInvalidModelOutputSkips(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(120)
	f.seedTurn(t, ctx, turnID, "ответь позже")

	// «Упавший процесс» с битым journal: turn_started + model_output, чей JSON
	// не проходит текущие правила (defer без defer_hours — до 17.8 писалось и
	// применялось молча, теперь apply разыменовывает DeferHours).
	if err := f.journal.WriteTurnStarted(ctx, &TurnStarted{
		TurnID: turnID, ChatID: testChatID, Kind: "conversation", BatchCount: 1, Model: "test",
	}); err != nil {
		t.Fatalf("turn_started не записан: %v", err)
	}
	if err := f.journal.WriteModelOutput(ctx, turnID, TurnResult{Decision: DecisionDefer}, nil, 0); err != nil {
		t.Fatalf("битой model_output не записан: %v", err)
	}

	// Скрипт пуст: recovery не должен доходить до модели.
	f.withModel()

	var out Outcome
	var runErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("recovery-путь паниковал на битом model_output (процесс бы умер): %v", p)
			}
		}()
		out, runErr = f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
			Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "ответь позже"}}})
	}()
	if runErr != nil {
		t.Fatalf("recovery с битым model_output должен завершаться пропуском, не ошибкой: %v", runErr)
	}
	if !out.Skipped {
		t.Fatalf("ход не помечен пропуском: %+v", out)
	}

	// Терминальный пропуск зафиксирован в журнале (фаза recovery, причина валидации).
	var skip *TurnSkipped
	evs, err := f.journal.History(ctx, turnID)
	if err != nil {
		t.Fatalf("журнал не читается: %v", err)
	}
	for _, ev := range evs {
		if s, ok := events.AsValue(ev).(TurnSkipped); ok {
			skip = &s
		}
	}
	if skip == nil {
		t.Fatalf("turn_skipped не записан: события %v", f.eventTypes(t, ctx, turnID))
	}
	if skip.Phase != "recovery" {
		t.Fatalf("фаза пропуска = %q, хочу recovery", skip.Phase)
	}
	if !strings.Contains(skip.Reason, "defer") && !strings.Contains(skip.Reason, "валид") {
		t.Fatalf("причина пропуска не называет валидацию: %q", skip.Reason)
	}

	// Ничего не применилось: outbox пуст, ход не applied.
	var outbox int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("битый результат применился к доставке: %d строк", outbox)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM turns WHERE id = $1`, turnID).Scan(&status); err != nil {
		t.Fatalf("turn не читается: %v", err)
	}
	if status == "applied" {
		t.Fatalf("битый результат применил turn")
	}
}

// TestTurnModelNotConfigured — без модели ход не выполняется и НЕ помечается
// терминальным пропуском (конфигурация придёт — ход повторится).
func TestTurnModelNotConfigured(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.Model = nil
	turnID := int64(105)
	f.seedTurn(t, ctx, turnID, "привет")

	_, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "привет"}}})
	if !errors.Is(err, ErrModelNotConfigured) {
		t.Fatalf("хочу ErrModelNotConfigured, получено: %v", err)
	}
	if types := f.eventTypes(t, ctx, turnID); len(types) != 0 {
		t.Fatalf("без модели не должно быть событий хода, есть %v", types)
	}
}

// TestTurnBudgetExhausted — исчерпание дневного бюджета (§16) прерывает ход
// до вызова модели; turn_started без model_output → повтор позже (§12.2).
func TestTurnBudgetExhausted(t *testing.T) {
	f, ctx := newFixture(t)
	f.runner.DailyCallsLimit = 1

	first := int64(106)
	f.seedTurn(t, ctx, first, "раз")
	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore","notes":["тихо"]}`}})
	if _, err := f.runner.Run(ctx, Input{TurnID: first, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "раз"}}}); err != nil {
		t.Fatalf("первый ход не выполнен: %v", err)
	}

	second := int64(107)
	f.seedTurn(t, ctx, second, "два")
	// Скрипт исчерпан намеренно: модель вызываться не должна.
	f.withModel()
	_, err := f.runner.Run(ctx, Input{TurnID: second, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, Text: "два"}}})
	if !errors.Is(err, runtime.ErrBudgetExhausted) {
		t.Fatalf("хочу ErrBudgetExhausted, получено: %v", err)
	}
	// turn_started есть, model_output нет — ход ждёт повторной попытки (§12.2).
	types := f.eventTypes(t, ctx, second)
	if len(types) != 1 || types[0] != EventTypeTurnStarted {
		t.Fatalf("события хода при исчерпании бюджета: %v, хочу только turn_started", types)
	}
}

// TestBudgetStoreRetryDoesNotDoubleReserve — retry той же операции не
// резервирует повторно (§16).
func TestBudgetStoreRetryDoesNotDoubleReserve(t *testing.T) {
	f, ctx := newFixture(t)

	ok, err := f.budget.Reserve(ctx, "model:turn:9:0:0", 0, runtime.BudgetPoolConversation, 1, 2)
	if err != nil || !ok {
		t.Fatalf("первый резерв не прошёл: %v %v", ok, err)
	}
	ok, err = f.budget.Reserve(ctx, "model:turn:9:0:0", 0, runtime.BudgetPoolConversation, 1, 2)
	if err != nil || !ok {
		t.Fatalf("retry той же операции должен пройти бесплатно: %v %v", ok, err)
	}
	// Разные операции считаются: 2 из лимита 2.
	ok, err = f.budget.Reserve(ctx, "model:turn:9:0:1", 0, runtime.BudgetPoolConversation, 1, 2)
	if err != nil || !ok {
		t.Fatalf("второй резерв не прошёл: %v %v", ok, err)
	}
	_, err = f.budget.Reserve(ctx, "model:turn:9:0:2", 0, runtime.BudgetPoolConversation, 1, 2)
	if !errors.Is(err, runtime.ErrBudgetExhausted) {
		t.Fatalf("лимит 2 не сработал: %v", err)
	}
	var reserved int64
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT reserved_calls FROM budget_counters WHERE pool = $1 AND period = CURRENT_DATE`,
		runtime.BudgetPoolConversation).Scan(&reserved); err != nil {
		t.Fatalf("счётчик не читается: %v", err)
	}
	if reserved != 2 {
		t.Fatalf("reserved_calls = %d, хочу 2 (retry не списывает)", reserved)
	}
}

// TestTurnStartedWrittenOnce — повторный прогон не дублирует turn_started.
func TestTurnStartedWrittenOnce(t *testing.T) {
	f, ctx := newFixture(t)
	ev := &TurnStarted{TurnID: 200, ChatID: testChatID, Kind: "conversation"}
	if err := f.journal.WriteTurnStarted(ctx, ev); err != nil {
		t.Fatalf("первая запись не прошла: %v", err)
	}
	ev2 := &TurnStarted{TurnID: 200, ChatID: testChatID, Kind: "conversation"}
	if err := f.journal.WriteTurnStarted(ctx, ev2); err != nil {
		t.Fatalf("повторная запись упала (должна быть no-op): %v", err)
	}
	types := f.eventTypes(t, ctx, 200)
	if len(types) != 1 {
		t.Fatalf("turn_started записан %d раз, хочу 1", len(types))
	}
}

// TestTurnAnswerWithReaction — answer с необязательной реакцией (§11.2):
// в outbox ложатся и сообщения, и реакция на последнее сообщение пачки.
func TestTurnAnswerWithReaction(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(111)
	// Событие с message.message_id в raw_update: цель реакции доступна.
	if _, err := f.runner.Pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id)
		VALUES (2000001, $1, 'private', 777, 'Коля', 'message', 'ну и новость',
		        '{"message":{"message_id":555}}'::jsonb, 'buffered', $2)`,
		testChatID, turnID); err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	if _, err := f.runner.Pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'created')`, turnID, testChatID); err != nil {
		t.Fatalf("turn не вставлен: %v", err)
	}

	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["Ха, согласен","Держи реакцию"],"reaction":"🤣"}`}})
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "ну и новость"}}})
	if err != nil {
		t.Fatalf("answer-ход с реакцией упал: %v", err)
	}
	if out.Apply.SkippedReaction {
		t.Fatalf("реакция при доступной цели не должна пропускаться: %+v", out)
	}
	if len(out.Apply.OutboxIDs) != 3 {
		t.Fatalf("в outbox должно быть 3 строки (2 сообщения + реакция), стало %d", len(out.Apply.OutboxIDs))
	}
	type row struct {
		position int
		kind     string
	}
	var rows []row
	rsets, err := f.runner.Pool.Query(ctx,
		`SELECT position, payload->>'kind' FROM delivery_outbox WHERE turn_id = $1 ORDER BY position`, turnID)
	if err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	for rsets.Next() {
		var r row
		if err := rsets.Scan(&r.position, &r.kind); err != nil {
			t.Fatalf("строка outbox не читается: %v", err)
		}
		rows = append(rows, r)
	}
	rsets.Close()
	want := []row{{0, "message"}, {1, "message"}, {2, "reaction"}}
	if len(rows) != len(want) {
		t.Fatalf("строки outbox: %+v, хочу %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("строка %d: %+v, хочу %+v", i, rows[i], want[i])
		}
	}
	var reaction, target string
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT payload->>'reaction', payload->>'target_message_id' FROM delivery_outbox WHERE turn_id = $1 AND payload->>'kind' = 'reaction'`,
		turnID).Scan(&reaction, &target); err != nil {
		t.Fatalf("реакция не читается: %v", err)
	}
	if reaction != "🤣" || target != "555" {
		t.Fatalf("реакция payload: reaction=%q target=%s, хочу 🤣/555", reaction, target)
	}
}

// TestTurnReactionTargetMissing — react без целевого сообщения: реакция
// пропущена, валидные дельты применяются (§11.2).
func TestTurnReactionTargetMissing(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(108)
	// Событие без message.message_id в raw_update: цель реакции недоступна.
	if _, err := f.runner.Pool.Exec(ctx, `
		INSERT INTO social_events (telegram_update_id, chat_id, chat_type, actor_id, actor_name, kind, text, raw_update, status, turn_id)
		VALUES (2000000, $1, 'private', 777, 'Коля', 'message', 'хм', '{}'::jsonb, 'buffered', $2)`,
		testChatID, turnID); err != nil {
		t.Fatalf("событие не вставлено: %v", err)
	}
	if _, err := f.runner.Pool.Exec(ctx,
		`INSERT INTO turns (id, chat_id, status) VALUES ($1, $2, 'created')`, turnID, testChatID); err != nil {
		t.Fatalf("turn не вставлен: %v", err)
	}

	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"react","reaction":"🤣"}`}})
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "хм"}}})
	if err != nil {
		t.Fatalf("react-ход упал: %v", err)
	}
	if !out.Apply.SkippedReaction {
		t.Fatalf("реакция без цели должна быть пропущена: %+v", out)
	}
	var outbox int
	if err := f.runner.Pool.QueryRow(ctx, `SELECT count(*) FROM delivery_outbox WHERE turn_id = $1`, turnID).Scan(&outbox); err != nil {
		t.Fatalf("outbox не читается: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("реакция без цели попала в outbox: %d строк", outbox)
	}
}

// TestTurnDeferCreatesIntention — defer создаёт reply_later намерение с
// notBefore (§11.2, §14.1); дубликат по dedupe не размножается.
func TestTurnDeferCreatesIntention(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(109)
	f.seedTurn(t, ctx, turnID, "давай позже обсудим кино")

	f.withModel(&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"defer","defer_hours":3,"notes":["вернусь к теме"]}`}})
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "давай позже обсудим кино"}}})
	if err != nil {
		t.Fatalf("defer-ход упал: %v", err)
	}
	if len(out.Apply.IntentionIDs) != 1 {
		t.Fatalf("defer должен создать 1 намерение, создано %d", len(out.Apply.IntentionIDs))
	}
	var kind, status string
	var notBefore time.Time
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT kind, status, not_before FROM intentions WHERE id = $1`,
		out.Apply.IntentionIDs[0]).Scan(&kind, &status, &notBefore); err != nil {
		t.Fatalf("намерение не читается: %v", err)
	}
	if kind != "reply_later" || status != "pending" {
		t.Fatalf("намерение kind=%s status=%s, хочу reply_later/pending", kind, status)
	}
	if !notBefore.After(time.Now().Add(2 * time.Hour)) {
		t.Fatalf("notBefore = %v, хочу через ~3 часа", notBefore)
	}
}

// TestTurnMemoryCandidateInvalidRejected — кандидат памяти, нарушающий §10.1,
// не проходит в транзакцию применения: невалидный результат не применяется.
func TestTurnMemoryCandidateInvalidRejected(t *testing.T) {
	f, ctx := newFixture(t)
	turnID := int64(110)
	f.seedTurn(t, ctx, turnID, "запиши: я из Ростова")

	// confidence у experience запрещён (§10.1) — валидатор должен это поймать
	// и уйти в repair, а repair вернёт чистый ответ.
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore","memory":[{"kind":"experience","text":"Коля из Ростова","confidence":0.9}]}`}},
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"ignore","memory":[{"kind":"claim","text":"Коля из Ростова","confidence":0.9}]}`}},
	)
	out, err := f.runner.Run(ctx, Input{TurnID: turnID, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, Text: "запиши: я из Ростова"}}})
	if err != nil {
		t.Fatalf("ход не выполнен: %v", err)
	}
	if len(out.Apply.MemoryIDs) != 1 {
		t.Fatalf("кандидат памяти не вставлен: %+v", out)
	}
	var memText string
	if err := f.runner.Pool.QueryRow(ctx,
		`SELECT text FROM memory_items WHERE id = $1`, out.Apply.MemoryIDs[0]).Scan(&memText); err != nil {
		t.Fatalf("запись памяти не читается: %v", err)
	}
	if !strings.Contains(memText, "Ростова") {
		t.Fatalf("вставился не repair-кандидат: %q", memText)
	}
}

// TestTurnHistoryReachesModel — история чата доходит до модели: реплики
// предыдущих ходов (входящие события + доставленные ответы) видны сценарному
// клиенту ПЕРЕД пачкой текущего хода (§11.1 п.5; находка 2026-10-31: History
// собирался, но не использовался — ход шёл с амнезией).
func TestTurnHistoryReachesModel(t *testing.T) {
	f, ctx := newFixture(t)

	// Ход 1: реплика пользователя, ответ применяется и доставляется.
	first := int64(9101)
	f.seedTurn(t, ctx, first, "я короче решил всё переписать")
	f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["а что переписываешь?"]}`}},
	)
	if _, err := f.runner.Run(ctx, Input{TurnID: first, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "я короче решил всё переписать"}}}); err != nil {
		t.Fatalf("ход 1 не выполнен: %v", err)
	}
	// Ответ доставлен: в историю попадают только sent-ответы (§13.5).
	if _, err := f.pool.Exec(ctx,
		`UPDATE delivery_outbox SET status='sent', telegram_message_id = 555000 WHERE turn_id = $1`, first); err != nil {
		t.Fatalf("outbox не доставлен: %v", err)
	}

	// Ход 2: новая пачка; модель должна увидеть ход 1 до пачки.
	second := int64(9102)
	f.seedTurn(t, ctx, second, "ну там пару сервисов")
	cfg2 := f.withModel(
		&events.ModelResponse{Message: &events.ModelMessage{Content: `{"decision":"answer","messages":["ну удачи)"]}`}},
	)
	if _, err := f.runner.Run(ctx, Input{TurnID: second, ChatID: testChatID, ScopeID: testScopeID,
		Batch: []BatchEvent{{EventID: 2, ActorID: 777, ActorName: "Коля", Text: "ну там пару сервисов"}}}); err != nil {
		t.Fatalf("ход 2 не выполнен: %v", err)
	}

	calls := cfg2.Calls()
	if len(calls) == 0 {
		t.Fatal("модель не вызывалась")
	}
	evs := calls[len(calls)-1]
	reqText := func(i int) string {
		req, ok := events.AsValue(evs[i]).(events.ModelRequest)
		if !ok {
			t.Fatalf("событие[%d] = %T, хочу ModelRequest", i, events.AsValue(evs[i]))
		}
		if len(req.Parts) == 0 {
			t.Fatalf("запрос[%d] без частей", i)
		}
		if ti, ok := events.AsValue(req.Parts[0]).(events.TextInput); ok {
			return ti.Content
		}
		t.Fatalf("часть[%d][0] не текст: %T", i, events.AsValue(req.Parts[0]))
		return ""
	}
	if len(evs) < 3 {
		t.Fatalf("на проводе %d событий, want >= 3 (реплика + ответ + пачка)", len(evs))
	}
	if got := reqText(0); !strings.HasSuffix(got, "] Коля: я короче решил всё переписать") {
		t.Fatalf("первая реплика истории без метки/автора: %q", got)
	}
	resp, ok := events.AsValue(evs[1]).(events.ModelResponse)
	if !ok || !strings.HasSuffix(resp.Content(), "] а что переписываешь?") {
		t.Fatalf("второе событие не ответ ассистента с меткой: %T %q", events.AsValue(evs[1]), resp.Content())
	}
	if got := reqText(2); !strings.HasPrefix(got, statePanelOpen) || !strings.HasSuffix(got, "ну там пару сервисов") {
		t.Fatalf("пачка не после истории/панели: %q", got)
	}
}
