// Ход (§11, §12): запуск ago-агента с восстановлением из журнала.
//
// Фазы (§12.1, §12.2):
//  0. чтение журнала хода: model_output есть — применение сохранённого
//     результата без новой генерации; deltas_applied есть — ход уже применён;
//  1. turn_started (однократно);
//  2. бюджет (§16) → контекст из БД (§11.1) → запуск агента ago: инструменты
//     памяти (§10.4), forced первый вызов на прямом обращении, лимит обращений
//     конфигурацией; строгая валидация TurnResult (§11.2), один repair-retry
//     (§11.3), невалидный результат не применяется — терминальный пропуск;
//  3. model_output записывается ДО применения (§12.1);
//  4. применение «всё или ничего» одной транзакцией (apply.go).
//
// Транзитные сбои (модель недоступна, бюджет исчерпан) не пишут терминальных
// событий: turn_started без model_output → ограниченный повтор с тем же
// stream_id (§12.2).
package character

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"

	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/runtime"
	"github.com/luckystrker/dio-agent/internal/telegram"
)

// sessionForChat — стабильный идентификатор разговора для провайдера: FNV-1a
// chat id, чтобы OpenCode Go маршрутизировал и кэшировал промпты одного чата
// вместе (сам chat id наружу не уходит, §17: id — только исполнение).
func sessionForChat(chatID int64) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("dio-chat-%d", chatID)))
	return fmt.Sprintf("dio-%016x", h.Sum64())
}

// wantForcedTool — принуждение первого вызова памяти: только прямой ход и
// только первая попытка. Повторы идут без forced tool_choice: thinking-режимы
// части провайдеров с ним несовместимы, а повторная генерация уже аварийная
// (портировано из TS-линии c0a986d, 2026-10-31).
func wantForcedTool(in Input, hasMemory bool) bool {
	return in.Direct && hasMemory && in.Attempt <= 1
}

// ErrModelNotConfigured — модель не настроена: ход не выполняется и не
// помечается терминальным пропуском (конфигурация приходит — ход повторится).
var ErrModelNotConfigured = errors.New("модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)")

// Runner — исполнитель ходов. Один когнитивный worker (§5.2) вызывает Run
// последовательно; методы безопасны и при параллельном использовании.
type Runner struct {
	Pool    *pgxpool.Pool
	Journal *TurnJournal
	Budget  *runtime.BudgetStore

	// Model — конфиг провайдера; nil — модель не настроена (ErrModelNotConfigured).
	Model *ChatConfig
	// DailyCallsLimit — дневной лимит модельных вызовов (§16); 0 — не ограничивать.
	DailyCallsLimit int64
	// ToolCallsLimit — лимит обращений к инструментам за ход (§11.2); 0 — без лимита.
	ToolCallsLimit int

	// Assembler — сборка базового контекста §11.1.
	Assembler *Assembler
	// Persona — активная персона (синхронизирована с файлами при старте §7.1).
	Persona Persona

	// Memory — инструментарий памяти (9.3); nil — ход без инструментов памяти.
	Memory *memory.Toolset
	// Photos — скачивание фото Telegram (vision §6.1); nil — модель видит текст.
	Photos PhotoFetcher
	// Typing — «печатает…» (§13.3); nil — не показывать.
	Typing *TypingPulse

	Logger *log.Logger
}

// Input — вход одного хода.
type Input struct {
	TurnID  int64
	ChatID  int64
	ScopeID string
	Batch   []BatchEvent
	// Direct — прямое обращение (приоритет 1 §5.2): первый вызов инструмента
	// памяти принудительный (§10.4).
	Direct bool
	// Attempt — номер попытки обработки (для детерминированного operation_id §16).
	Attempt int
}

// Outcome — результат обработки хода.
type Outcome struct {
	// Recovered — применён сохранённый результат без новой генерации (§12.2).
	Recovered bool
	// Skipped — терминальный пропуск (turn_skipped, инвариант 5).
	Skipped    bool
	SkipReason string
	// AlreadyApplied — ход уже применён ранее; повторный прогон ничего не менял.
	AlreadyApplied bool
	// Apply — итог транзакции применения.
	Apply ApplyResult
	// LLMCalls — сколько модельных вызовов сделал ход (наблюдаемость §20).
	LLMCalls int
}

// Run обрабатывает ход по фазам §12.1–12.2.
func (r *Runner) Run(ctx context.Context, in Input) (Outcome, error) {
	out := Outcome{}

	// --- Фаза 0: восстановление из журнала (§12.2) ---
	state, err := LoadTurnState(ctx, r.Journal, in.TurnID)
	if err != nil {
		return out, fmt.Errorf("ход %d: журнал не читается: %w", in.TurnID, err)
	}
	if state.Applied {
		out.AlreadyApplied = true
		r.logf("ход %d уже применён (deltas_applied в журнале) — повтор невозможен", in.TurnID)
		return out, nil
	}
	if state.Skipped {
		out.Skipped = true
		out.SkipReason = state.SkipReason
		r.logf("ход %d терминально пропущен ранее (%s)", in.TurnID, state.SkipReason)
		return out, nil
	}
	if state.Output != nil {
		// model_output без deltas_applied → применить сохранённый результат
		// без нового LLM (§12.2, требование 9.7).
		r.logf("ход %d: восстановление — применяю сохранённый результат без новой генерации", in.TurnID)
		out.Recovered = true
		return r.apply(ctx, in, *state.Output, out)
	}

	// --- Фаза 1: модель и turn_started ---
	if r.Model == nil {
		return out, ErrModelNotConfigured
	}
	// operation префикса бюджета (§16): детерминирован на попытку обработки.
	operation := fmt.Sprintf("model:turn:%d:%d", in.TurnID, in.Attempt)
	if err := r.Journal.WriteTurnStarted(ctx, &TurnStarted{
		TurnID:       in.TurnID,
		ChatID:       in.ChatID,
		Kind:         "conversation",
		BatchCount:   len(in.Batch),
		Direct:       in.Direct,
		Model:        r.Model.Model(),
		BudgetOpID:   operation,
		ContextRevis: 0, // ревизия контекста уточняется при edit/delete (§19)
	}); err != nil {
		return out, fmt.Errorf("ход %d: turn_started не записан: %w", in.TurnID, err)
	}

	// --- Фаза 2: контекст §11.1 и запуск агента ---
	bctx, err := r.Assembler.Assemble(ctx, r.Pool, r.Persona, in.ScopeID, in.ChatID, in.Batch, in.TurnID)
	if err != nil {
		return out, fmt.Errorf("ход %d: контекст не собран: %w", in.TurnID, err)
	}

	userText := strings.Join(bctx.Batch, "\n")
	if strings.TrimSpace(userText) == "" {
		userText = "[сообщение без текста]"
	}

	// Фото пачки (vision §6.1): скачивание до вызова, ошибки — ход по тексту.
	photos := CollectPhotos(ctx, in.Batch, r.Photos, telegram.MaxImagesPerTurn)
	result, repairs, llmCalls, err := r.generate(ctx, in, bctx, userText, operation, photos)
	out.LLMCalls = llmCalls
	var invalidErr *invalidResultError
	if errors.As(err, &invalidErr) {
		// Результат так и не стал валидным: терминальный пропуск с причиной
		// (§11.3: после исчерпания попыток — skipped, система продолжает приём).
		reason := fmt.Sprintf("валидация TurnResult не пройдена после repair: %v", invalidErr.err)
		if skipErr := r.Journal.WriteTurnSkipped(ctx, in.TurnID, "generate", reason); skipErr != nil {
			return out, fmt.Errorf("ход %d: turn_skipped не записан: %w", in.TurnID, skipErr)
		}
		out.Skipped = true
		out.SkipReason = reason
		r.logf("ход %d: %s", in.TurnID, reason)
		return out, nil
	}
	if err != nil {
		// Транзитный сбой генерации: turn_started без model_output → повтор (§12.2).
		return out, fmt.Errorf("ход %d: генерация не удалась: %w", in.TurnID, err)
	}

	// --- Фаза 3: model_output ДО применения (§12.1) ---
	if err := r.Journal.WriteModelOutput(ctx, in.TurnID, result, nil, repairs); err != nil {
		return out, fmt.Errorf("ход %d: model_output не записан: %w", in.TurnID, err)
	}

	// --- Фаза 4: применение одной транзакцией ---
	return r.apply(ctx, in, result, out)
}

// invalidResultError — ответ модели не прошёл валидацию §11.2 ни с первой,
// ни с repair-попытки.
type invalidResultError struct{ err error }

func (e *invalidResultError) Error() string { return e.err.Error() }

// generate запускает ago-агента и добивается валидированного TurnResult
// (не более одного repair-retry, §11.3). Возвращает результат, число
// repair-попыток и число модельных вызовов.
func (r *Runner) generate(ctx context.Context, in Input, bctx BaseContext, userText, operation string, photos []Photo) (TurnResult, int, int, error) {
	// Свой конфиг на ход: все запуски агента внутри хода (первый Ask и
	// repair-продолжение через reply.Ask) делят один счётчик вызовов, поэтому
	// forced tool_choice встаёт ровно на первый вызов хода, а repair §11.3
	// получает обычный вызов и может ответить финальным TurnResult.
	cfg := &ChatConfig{params: r.Model.params, inner: r.Model.inner, SessionID: sessionForChat(in.ChatID)}
	if wantForcedTool(in, r.Memory != nil) {
		// Forced tool-calling на прямом обращении (§10.4): первый вызов модели
		// обязан обратиться к forced-инструменту памяти — гарантия «прямой
		// вопрос всегда получает нужную память» держится механикой провайдера.
		cfg.ForcedTool = memory.ToolSearch
	}

	opts := []agent.Option{
		agent.WithConfig(cfg),
		// Системный промпт из шаблона §7.1; constraints — отдельная структурная
		// секция контекста (§17), не смешанная с данными пачки.
		agent.WithPromptFragments(bctx.Constraints),
		agent.WithResponseSchema(response.RawSchema(TurnResultJSONSchema(),
			response.WithRawName("TurnResult"),
			response.WithRawDescription("Цельный результат хода: решение, сообщения, кандидаты памяти, дельты состояния."))),
	}
	if r.Memory != nil {
		opts = append(opts, agent.WithTools(r.Memory.Tools()...))
	}
	// История чата (§11.1 п.5): реплики прошлых ходов — настоящими user/
	// assistant сообщениями до пачки, append-only (кэш префикса провайдера
	// не страдает). Пустая история — поток по умолчанию.
	if len(bctx.History) > 0 {
		seeded, err := historyStream(bctx.History)
		if err != nil {
			return TurnResult{}, 0, 0, fmt.Errorf("ход %d: история чата не посеяна: %w", in.TurnID, err)
		}
		opts = append(opts, agent.WithStreamBase(seeded))
	}

	// Middleware: бюджет §16 — внешний слой (резерв до всего остального);
	// журналирование tool_call/tool_result — наружу лимита, чтобы отклонённые
	// лимитом вызовы тоже были зафиксированы; typing §13.3 — на весь ход.
	budgetMW := NewBudgetMiddleware(r.Budget, operation, r.DailyCallsLimit, r.Logger)
	toolMW := NewToolLimitMiddleware(r.ToolCallsLimit, r.Logger)
	mws := []agomiddleware.Factory{func() agomiddleware.Middleware { return budgetMW }}
	if r.Memory != nil {
		mws = append(mws,
			func() agomiddleware.Middleware { return &toolJournalMW{runner: r, turnID: in.TurnID} },
			func() agomiddleware.Middleware { return toolMW },
		)
	}
	if r.Typing != nil {
		mws = append(mws, func() agomiddleware.Middleware { return NewTypingMiddleware(r.Typing, in.ChatID) })
	}
	opts = append(opts, agent.WithMiddleware(mws...))
	// DI аудитории хода: инструменты памяти фильтруют приватность до выдачи (§9.2).
	opts = append(opts, agent.WithDependencies(map[string]any{memory.ScopeDependency: in.ScopeID}))

	a := agent.New("dio", bctx.System, opts...)
	// Части Ask: текст пачки + фото BinaryInput (находка приёмки Р: фото
	// скачивалось, но в вызов модели уходил только текст).
	reply, err := a.Ask(ctx, askMsgs(userText, photos)...)
	if err != nil {
		return TurnResult{}, 0, budgetMW.callCount(), err
	}

	// --- строгая валидация ответа (§11.2); один repair-retry (§11.3) ---
	repairs := 0
	result, perr := parseReply(reply)
	if perr != nil {
		repairs = 1
		fixed, ferr := reply.Ask(ctx,
			fmt.Sprintf("Твой последний ответ не прошёл валидацию: %v. Верни строго один JSON-объект по схеме TurnResult, без текста вокруг.", perr))
		if ferr != nil {
			return TurnResult{}, repairs, budgetMW.callCount(), ferr
		}
		result, perr = parseReply(fixed)
		if perr != nil {
			return TurnResult{}, repairs, budgetMW.callCount(), &invalidResultError{err: perr}
		}
	}
	return result, repairs, budgetMW.callCount(), nil
}

// parseReply разбирает тело ответа как строгий TurnResult (§11.2).
func parseReply(reply *agent.AgentReply) (TurnResult, error) {
	body, err := reply.Content()
	if err != nil {
		return TurnResult{}, err
	}
	return ParseTurnResult(body)
}

// toolJournalMW — middleware журналирования инструментного цикла (§12.1:
// «каждое обращение к инструменту и его результат фиксируются событиями журнала»).
type toolJournalMW struct {
	agomiddleware.Base
	runner *Runner
	turnID int64
}

// OnToolExecution пишет tool_call до исполнения и tool_result после; сбои
// журнала не роняют ход (журнал — наблюдаемость, ход продолжается), но факты
// исполнения инструментов всегда видны в журнале или в логе.
func (m *toolJournalMW) OnToolExecution(next agomiddleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	ctx := context.Background()
	if err := m.runner.Journal.WriteToolCallFor(ctx, m.turnID,
		&ToolCallRecorded{CallID: ev.ID, Tool: ev.Name, Arguments: ev.Arguments}); err != nil {
		m.runner.logf("ход %d: tool_call не записан: %v", m.turnID, err)
	}
	result, err := next(ev, cx)
	rec := &ToolResultRecorded{CallID: ev.ID, Tool: ev.Name}
	switch out := events.AsValue(result).(type) {
	case events.ToolResultEvent:
		texts := make([]string, 0, len(out.Result.Parts))
		for _, p := range out.Result.Parts {
			switch part := events.AsValue(p).(type) {
			case events.TextInput:
				texts = append(texts, part.Content)
			default:
				b, _ := json.Marshal(part)
				texts = append(texts, string(b))
			}
		}
		rec.Result = strings.Join(texts, "\n")
	case events.ToolErrorEvent:
		rec.Error = out.Error
	}
	if werr := m.runner.Journal.WriteToolResultFor(ctx, m.turnID, rec); werr != nil {
		m.runner.logf("ход %d: tool_result не записан: %v", m.turnID, werr)
	}
	return result, err
}

// apply — фаза 4: применение валидированного результата (одна транзакция).
func (r *Runner) apply(ctx context.Context, in Input, result TurnResult, out Outcome) (Outcome, error) {
	applyRes, err := ApplyTurn(ctx, r.Pool, ApplyOptions{
		TurnID:          in.TurnID,
		ChatID:          in.ChatID,
		ScopeID:         in.ScopeID,
		Result:          result,
		PersonaBaseline: r.Persona.MoodBaseline,
		Journal:         r.Journal,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrHistoryConflict) {
			// События применения уже в журнале (ход применил другой процесс):
			// повторный прогон не дублирует (§12.1).
			out.AlreadyApplied = true
			return out, nil
		}
		return out, fmt.Errorf("ход %d: применение не удалось: %w", in.TurnID, err)
	}
	out.Apply = applyRes
	if !applyRes.Applied {
		out.AlreadyApplied = true
	}
	// Индексация эмбеддингов (§10.4, 16.8): применённый ход векторизует новые
	// записи памяти — семантический канал оживает в прод-пути. Лучшие усилия:
	// ошибка индексации не трогает применённый ход, векторы догонят позже.
	if applyRes.Applied && r.Memory != nil {
		if n, ierr := r.Memory.IndexPending(ctx, 0); ierr != nil {
			r.logf("ход %d: индексация памяти отложена: %v", in.TurnID, ierr)
		} else if n > 0 {
			r.logf("ход %d: проиндексировано записей памяти: %d", in.TurnID, n)
		}
	}
	r.logf("ход %d выполнен: decision=%s сообщений=%d памяти=%d намерений=%d доставки=%d recovered=%v",
		in.TurnID, result.Decision, len(result.Messages), len(out.Apply.MemoryIDs),
		len(out.Apply.IntentionIDs), len(out.Apply.OutboxIDs), out.Recovered)
	return out, nil
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}
