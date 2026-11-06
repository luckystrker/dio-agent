// Модельные вызовы фоновой жизни (§14, §15): инициатива и exploration дают
// строгий TurnResult (та же схема §11.2, один repair-retry §11.3), консолидация
// — свой proposal-контракт (модель только предлагает, применяет runtime §10.2).
// Все вызовы ходят в пуле бюджета background (§16: исследование не тратит
// бюджет прямых разговоров), operation_id детерминирован на попытку.
// Инструменты — только память (§10.4, §15 MVP: без web — их просто нет).
package background

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/luckystrker/ago/agent"
	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	agomiddleware "github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/response"
	agotools "github.com/luckystrker/ago/tools"

	"github.com/luckystrker/dio-agent/internal/character"
	"github.com/luckystrker/dio-agent/internal/memory"
	"github.com/luckystrker/dio-agent/internal/modebug"
	"github.com/luckystrker/dio-agent/internal/runtime"
)

// Addenda к системному промпту фоновой жизни (паритет TS-справочника).
const (
	initiativeAddendum = "Ты сам решил написать первым (инициатива). Будь уместен и кратко: одна мысль, без дежурных извинений. " +
		"Если за это время собеседник уже писал тебе на другую тему — лучше ignore."

	exploreAddendum = "Ты изучаешь тему для себя (exploration). НИЧЕГО не отправляй собеседнику: " +
		"decision=ignore, а находки оформи в memory (thought/preference/claim о теме). " +
		"Ответ держи коротким: не больше 3 кандидатов памяти, каждый — до 400 символов; длинные конспекты не нужны — финал может оборваться по лимиту токенов."
)

// Ограничения proposal-контракта консолидации (§10.1: runtime проверяет
// типы и длины; значения — начальные пределы спеки).
const (
	maxSummaryLength  = 2000
	maxStanceAspects  = 5
	maxEntityMerges   = 5
	maxAspectLength   = 100
	maxEntityNameLen  = 200
	maxMergeReasonLen = 300
	episodeItemLimit  = 30  // записей памяти в промпт консолидации
	episodeTextLimit  = 300 // символов события в промпте консолидации
)

// Потолки explore на задачу (этап 38.8): живой прогон 2026-11-06 дошёл до
// 9 модельных вызовов и 78 705 токенов на мусорную тему — задача обязана
// заканчиваться тем, что уже найдено, а не крутиться до лимита инструментов.
const (
	exploreMaxCalls  = 6
	exploreMaxTokens = 40_000
)

// modelCaps — потолок одной фоновой задачи. Нули — без потолка.
type modelCaps struct {
	Calls  int
	Tokens int64
}

// errInvalidResult — ответ модели не прошёл валидацию ни с первой, ни с
// repair-попытки: терминальный случай (повтор с тем же контекстом бессмыслен).
type errInvalidResult struct{ err error }

func (e *errInvalidResult) Error() string { return e.err.Error() }

// modelStats — фактическая стоимость фоновой задачи (§20: usage в записи хода).
type modelStats struct {
	Calls  int
	Tokens int64
}

// usageCounter — наблюдаемость модельного цикла фона: число вызовов и токены.
// Потолок задачи (этап 38.8): при достижении capCalls/capTokens один раз
// зовётся onCap — харнесс выключает инструменты, и модель обязана выдать
// финальный TurnResult с тем, что уже найдено.
type usageCounter struct {
	agomiddleware.Base
	mu        sync.Mutex
	calls     int
	tokens    int64
	capCalls  int
	capTokens int64
	capped    bool
	onCap     func()
}

func (u *usageCounter) OnLLMCall(next agomiddleware.LLMCallFunc, evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
	resp, err := next(evs, cx)
	u.mu.Lock()
	u.calls++
	if resp != nil {
		if resp.Usage.PromptTokens != nil {
			u.tokens += *resp.Usage.PromptTokens
		}
		if resp.Usage.CompletionTokens != nil {
			u.tokens += *resp.Usage.CompletionTokens
		}
	}
	overCalls := u.capCalls > 0 && u.calls >= u.capCalls
	overTokens := u.capTokens > 0 && u.tokens >= u.capTokens
	fire := (overCalls || overTokens) && !u.capped
	if overCalls || overTokens {
		u.capped = true
	}
	onCap := u.onCap
	u.mu.Unlock()
	if fire && onCap != nil {
		onCap()
	}
	return resp, err
}

func (u *usageCounter) stats() modelStats {
	u.mu.Lock()
	defer u.mu.Unlock()
	return modelStats{Calls: u.calls, Tokens: u.tokens}
}

// turnModelRun — результат модельного вызова с TurnResult-контрактом.
type turnModelRun struct {
	Result  character.TurnResult
	Repairs int
	Stats   modelStats
	// Reasoning/ReasoningRepair — рассуждение модели первой и repair-попытки
	// (26.3): журнал фонового хода, в контекст не возвращается.
	Reasoning       string
	ReasoningRepair string
}

// runTurnModel выполняет один фоновый ход (§11.2): strict TurnResult, один
// repair-retry (§11.3), резерв бюджета до вызова (§16, пул background).
// tools — только инструменты памяти; scopeID — DI-аудитория (§9.2). На
// инициативных ходах обращение к памяти не принуждается (§10.4).
// runTurnModel — модельный ход фоновой задачи: строгий TurnResult, один
// repair-retry (§11.3), резерв бюджета до вызова (§16, пул background), сессия
// провайдера по чату (x-opencode-session). sessionID пуст — заголовка нет.
// cognitionGate — семафор когниции на каждый модельный вызов (§5.2): фон не
// держит общий замок на всё поколение (несколько вызовов инструментов и
// repair), поэтому прямой ход может вклиниться между ними. nil-замок — no-op.
type cognitionGate struct {
	agomiddleware.Base
	lock *runtime.CognitionLock
}

func (g *cognitionGate) OnLLMCall(next agomiddleware.LLMCallFunc, evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
	release, ok := g.lock.Acquire(context.Background())
	if !ok {
		return nil, context.Canceled
	}
	defer release()
	return next(evs, cx)
}

func (s *Scheduler) cognitionMiddleware() agomiddleware.Middleware {
	return &cognitionGate{lock: s.cognition}
}

func (s *Scheduler) runTurnModel(ctx context.Context, operation, system, userText string, tools []agotools.Tool, scopeID, sessionID string, extraMW []agomiddleware.Factory) (turnModelRun, error) {
	return s.runTurnModelCapped(ctx, operation, system, userText, tools, scopeID, sessionID, modelCaps{}, extraMW)
}

// runTurnModelCapped — то же с потолком задачи (38.8): исчерпание вызовов или
// токенов выключает инструменты на конфиге хода (тот же приём, что лимит
// обращений §11.2), следующий модельный вызов обязан выдать финальный
// TurnResult с уже найденным.
func (s *Scheduler) runTurnModelCapped(ctx context.Context, operation, system, userText string, tools []agotools.Tool, scopeID, sessionID string, caps modelCaps, extraMW []agomiddleware.Factory) (turnModelRun, error) {
	// Отладочный вызов модели (этап 40): имя фоновой задачи едет в context —
	// дамп 40.1 и reasoning_content из транспорта (40.3) без глобалов.
	call := modebug.NewCall("bg", dumpTask(operation))
	ctx = modebug.WithCall(ctx, call)
	overrides := map[string]any(nil)
	if sessionID != "" {
		overrides = map[string]any{"session_id": sessionID}
	}
	cfg, err := s.model.Clone(overrides)
	if err != nil {
		return turnModelRun{}, fmt.Errorf("фон: конфиг модели не клонирован: %w", err)
	}

	fragments := []string{
		"Ограничения хода (runtime, не обсуждаемое):",
		"- Финальный ответ — только JSON по схеме TurnResult, без текста вокруг него.",
		"- decision=answer: от 1 до 4 сообщений, каждое не длиннее 4000 символов.",
		"- ignore — без сообщений; результаты инструментов памяти — контекст, а не отправляемый текст.",
		"- " + character.DelayHoursHint + ".",
	}

	// Clone возвращает *character.ChatConfig; на нём выключаем инструменты,
	// когда лимит исчерпан (bug 2026-11-02) — следующий вызов идёт без tools.
	toolMW := character.NewToolLimitMiddleware(s.toolLimit, s.logger)
	chatCfg, ok := cfg.(*character.ChatConfig)
	if !ok {
		// s.model — *character.ChatConfig, его Clone всегда возвращает тот же
		// тип; другая реализация сломала бы выключение инструментов молча.
		return turnModelRun{}, fmt.Errorf("фон: конфиг модели не *character.ChatConfig: %T", cfg)
	}
	toolMW.SetOnExhausted(chatCfg.DisableTools)

	counting := &usageCounter{capCalls: caps.Calls, capTokens: caps.Tokens}
	if caps.Calls > 0 || caps.Tokens > 0 {
		counting.onCap = func() {
			chatCfg.DisableTools()
			// Middleware тоже знает о закрытии: иначе упорный вызов
			// инструмента после потолка исполнился бы (39.1/39.2).
			toolMW.SetClosed()
			s.logInfo("потолок фоновой задачи достигнут (%d вызовов / %d токенов) — дальше без инструментов, финал с тем, что есть",
				caps.Calls, caps.Tokens)
		}
	}
	reasoning := character.NewReasoningCapture()
	opts := []agent.Option{
		agent.WithConfig(cfg),
		// Constraints — отдельная структурная секция (§17), не смешанная с данными.
		agent.WithPromptFragments(fragments...),
		agent.WithResponseSchema(response.RawSchema(character.TurnResultJSONSchema(),
			response.WithRawName("TurnResult"),
			response.WithRawDescription("Цельный результат фонового хода: решение, сообщения, кандидаты памяти."))),
		agent.WithMiddleware(
			// Когниция — на каждый модельный вызов, а не на всё поколение:
			// прямой ход может вклиниться между вызовами инструментов (M1).
			s.cognitionMiddleware,
			// Бюджет — внешний слой: резерв до всего остального (§16).
			func() agomiddleware.Middleware {
				return character.NewBudgetMiddlewarePool(s.budget, operation, s.dailyLimit,
					runtime.BudgetPoolBackground, s.logger)
			},
			func() agomiddleware.Middleware { return counting },
			func() agomiddleware.Middleware { return toolMW },
			// Reasoning (26.3) — отдельно от истории, только для журнала.
			func() agomiddleware.Middleware { return reasoning },
		),
		agent.WithDependencies(map[string]any{
			memory.ScopeDependency:       scopeID,
			memory.SearchStateDependency: memory.NewSearchState(),
		}),
	}
	if len(tools) > 0 {
		opts = append(opts, agent.WithTools(tools...))
	}
	if len(extraMW) > 0 {
		opts = append(opts, agent.WithMiddleware(extraMW...))
	}

	ag := agent.New("dio-bg", system, opts...)
	first, err := ag.Ask(ctx, userText)
	if err != nil {
		return turnModelRun{Stats: counting.stats()}, err
	}
	reasoningOut := turnModelRun{Stats: counting.stats(), Reasoning: modebug.TakeReasoning(call, reasoning.Take())}

	result, perr := parseTurnReply(first)
	validationRepair := false
	if perr != nil {
		// Один repair-retry (§11.3): тот же контекст, модели показана ошибка.
		fixed, ferr := first.Ask(ctx, character.RepairHint(perr, "TurnResult"))
		if ferr != nil {
			return reasoningOut, ferr
		}
		reasoningOut.ReasoningRepair = modebug.TakeReasoning(call, reasoning.Take())
		result, perr = parseTurnReply(fixed)
		validationRepair = true
		reasoningOut.Repairs = 1
	}

	// 40.4: валидный JSON, но messages — брак (как у разговорного хода):
	// один repair «в messages — только текст», иначе чистка первого ответа.
	if perr == nil && result.Decision == character.DecisionAnswer {
		if bad := character.GarbageReasons(result.Messages); len(bad) > 0 {
			s.logInfo("фон %s: брак в messages (%s) — repair 40.4", operation, strings.Join(bad, "; "))
			firstAnswer := result
			attempted := false
			var dropped []string
			result, _, dropped = character.FixGarbageMessages(firstAnswer, !validationRepair, func() (character.TurnResult, error) {
				fixed, ferr := first.Ask(ctx, character.GarbageRepairHint)
				if ferr != nil {
					return character.TurnResult{}, ferr
				}
				attempted = true
				reasoningOut.ReasoningRepair = modebug.TakeReasoning(call, reasoning.Take())
				return parseTurnReply(fixed)
			})
			if attempted {
				reasoningOut.Repairs = 1
			}
			if len(dropped) > 0 {
				s.logInfo("фон %s: бракованные сообщения выкинуты (%s), осталось %d; decision=%s",
					operation, strings.Join(dropped, "; "), len(result.Messages), result.Decision)
			}
		}
	}
	if perr != nil {
		return reasoningOut, &errInvalidResult{err: perr}
	}
	reasoningOut.Result = result
	return reasoningOut, nil
}

// parseTurnReply разбирает тело ответа как строгий TurnResult (§11.2).
func parseTurnReply(reply *agent.AgentReply) (character.TurnResult, error) {
	body, err := reply.Content()
	if err != nil {
		return character.TurnResult{}, err
	}
	return character.ParseTurnResult(body)
}

// runConsolidationModel — модельный вызов консолидации (§10.2): proposal по
// своей схеме, один repair (§11.3); инструменты не выдаются — модель только
// предлагает, ничего не ищет и не отправляет. Сессия провайдера по чату
// эпизода (x-opencode-session, как у прямого хода; находка 2026-10-31: вызовы
// без заголовка падали HTTP 400 MissingSessionID). sessionID пуст — заголовка нет.
func (s *Scheduler) runConsolidationModel(ctx context.Context, operation, system, userText, sessionID string) (consolidationProposal, modelStats, error) {
	overrides := map[string]any(nil)
	if sessionID != "" {
		overrides = map[string]any{"session_id": sessionID}
	}
	cfg, err := s.model.Clone(overrides)
	if err != nil {
		return consolidationProposal{}, modelStats{}, fmt.Errorf("фон: конфиг модели не клонирован: %w", err)
	}
	// Дамп модельных вызовов (40.1): консолидация тоже видна в MODEL_DEBUG_DUMP_DIR.
	ctx = modebug.WithCall(ctx, modebug.NewCall("bg", dumpTask(operation)))

	fragments := []string{
		"Ограничения (runtime, не обсуждаемое):",
		"- Финальный ответ — только JSON по схеме ConsolidationProposal, без текста вокруг него.",
		"- summary — только из содержания эпизода; ничего не выдумывай сверх записанного.",
		"- stances — только по перечисленным сущностям эпизода; strength/interest в границах 0..1.",
		"- entity_merges — только если в эпизоде явно видно, что два имени — одна и та же сущность; keep — оставляемое имя, merge — поглощаемое, reason — основание. Разные Telegram-аккаунты (user:<id>) — разные люди: не склеивать. При сомнении не склеивай (§8.5).",
		"- Повторное чтение тех же источников не повышает устойчивость позиции (strength) — оценивай содержание, а не количество записей (§10.3).",
		"- Предположение не превращается в подтверждённый факт.",
	}

	counting := &usageCounter{}
	a := agent.New("dio-consolidate", system,
		agent.WithConfig(cfg),
		agent.WithPromptFragments(fragments...),
		agent.WithResponseSchema(response.RawSchema(consolidationJSONSchema(),
			response.WithRawName("ConsolidationProposal"),
			response.WithRawDescription("Предложение консолидации: summary эпизода и пересмотры stances."))),
		agent.WithMiddleware(
			s.cognitionMiddleware,
			func() agomiddleware.Middleware {
				return character.NewBudgetMiddlewarePool(s.budget, operation, s.dailyLimit,
					runtime.BudgetPoolBackground, s.logger)
			},
			func() agomiddleware.Middleware { return counting },
		),
	)

	first, err := a.Ask(ctx, userText)
	if err != nil {
		return consolidationProposal{}, counting.stats(), err
	}
	body, cerr := first.Content()
	if cerr != nil {
		return consolidationProposal{}, counting.stats(), cerr
	}
	p, perr := parseConsolidationProposal(body)
	if perr == nil {
		return p, counting.stats(), nil
	}

	// Один repair-retry (§11.3): тот же контекст, модели показана ошибка.
	fixed, ferr := first.Ask(ctx, character.RepairHint(perr, "ConsolidationProposal"))
	if ferr != nil {
		return consolidationProposal{}, counting.stats(), ferr
	}
	fbody, cerr := fixed.Content()
	if cerr != nil {
		return consolidationProposal{}, counting.stats(), cerr
	}
	fp, fperr := parseConsolidationProposal(fbody)
	if fperr != nil {
		return consolidationProposal{}, counting.stats(), &errInvalidResult{err: fperr}
	}
	return fp, counting.stats(), nil
}

// --- proposal-контракт консолидации (§10.2) ----------------------------------

// stanceProposal — предложение пересмотра отношения. Модель оперирует ИМЕНАМИ
// сущностей (id назначает runtime) и не ссылается на записи: основания
// (stance_basis) выбирает runtime из памяти эпизода (§10.1).
type stanceProposal struct {
	Entity   string  `json:"entity"`
	Aspect   string  `json:"aspect"`
	Summary  string  `json:"summary"`
	Strength float64 `json:"strength"`
	Interest float64 `json:"interest"`
}

// entityMergeProposal — предложение «A = B» (§8.5): keep — выжившее имя,
// merge — поглощаемое, reason — основание. Имена резолвит и проверяет runtime;
// модель не оперирует id и не решает, склеивать ли.
type entityMergeProposal struct {
	Keep   string `json:"keep"`
	Merge  string `json:"merge"`
	Reason string `json:"reason"`
}

// consolidationProposal — ответ модели консолидации: краткое summary эпизода
// и предложения stances. Confidence модель не предлагает вовсе: summary —
// experience без confidence, повторное чтение источника не повышает уверенность
// (§10.2, §10.3). EntityMerges — необязательные предложения объединения
// сущностей эпизода (§8.5): в TurnResult их нет — склейка обслуживающая
// операция, а разговорный ход не видит эпизод целиком.
type consolidationProposal struct {
	Summary      string                `json:"summary"`
	Stances      []stanceProposal      `json:"stances,omitempty"`
	EntityMerges []entityMergeProposal `json:"entity_merges,omitempty"`
}

func consolidationJSONSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"summary": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   maxSummaryLength,
				"description": "Краткое фактичное резюме эпизода разговора: о чём говорили и чем кончилось.",
			},
			"stances": map[string]any{
				"type":     "array",
				"maxItems": maxStanceAspects,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"entity": map[string]any{
							"type":        "string",
							"description": "Имя сущности — ТОЛЬКО из списка сущностей эпизода.",
						},
						"aspect": map[string]any{
							"type":        "string",
							"description": "Аспект отношения (например, general, вкус, надёжность).",
						},
						"summary": map[string]any{
							"type":        "string",
							"description": "Текущее отношение к сущности по аспекту, одной фразой.",
						},
						"strength": map[string]any{
							"type":        "number",
							"description": "Устойчивость позиции 0..1. Повторное чтение тех же источников устойчивость не повышает.",
						},
						"interest": map[string]any{
							"type":        "number",
							"description": "Желание возвращаться к теме 0..1.",
						},
					},
					"required": []string{"entity", "aspect", "summary", "strength", "interest"},
				},
			},
			"entity_merges": map[string]any{
				"type":        "array",
				"maxItems":    maxEntityMerges,
				"description": "Необязательные предложения объединения сущностей эпизода. Только при явной уверенности, что это одна и та же сущность; при сомнении список пуст.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"keep": map[string]any{
							"type":        "string",
							"description": "Имя выживающей сущности — из списка сущностей эпизода.",
						},
						"merge": map[string]any{
							"type":        "string",
							"description": "Имя поглощаемой сущности — из списка сущностей эпизода.",
						},
						"reason": map[string]any{
							"type":        "string",
							"description": "Основание: почему это одна и та же сущность (не просто похожие имена).",
						},
					},
					"required": []string{"keep", "merge", "reason"},
				},
			},
		},
		"required": []string{"summary"},
	}
}

// parseConsolidationProposal валидирует предложение консолидации: строки и
// длины проверяются, strength/interest за границей 0..1 сжимаются до границы
// (модель предлагает — runtime применяет; паритет сжатия дельт настроения).
func parseConsolidationProposal(raw []byte) (consolidationProposal, error) {
	raw = character.EscapeJSONControlChars(raw)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p consolidationProposal
	if err := dec.Decode(&p); err != nil {
		return consolidationProposal{}, fmt.Errorf("консолидация: proposal: %w", err)
	}
	if dec.More() {
		return consolidationProposal{}, errors.New("консолидация: после JSON-объекта есть лишний текст")
	}

	p.Summary = strings.TrimSpace(p.Summary)
	if p.Summary == "" {
		return consolidationProposal{}, errors.New("консолидация: пустое summary")
	}
	if len([]rune(p.Summary)) > maxSummaryLength {
		return consolidationProposal{}, fmt.Errorf("консолидация: summary длиннее %d символов", maxSummaryLength)
	}
	if len(p.Stances) > maxStanceAspects {
		return consolidationProposal{}, fmt.Errorf("консолидация: больше %d предложений stances", maxStanceAspects)
	}
	for i := range p.Stances {
		sp := &p.Stances[i]
		sp.Entity = strings.TrimSpace(sp.Entity)
		if sp.Entity == "" {
			return consolidationProposal{}, fmt.Errorf("консолидация: stance #%d без сущности", i)
		}
		if len([]rune(sp.Entity)) > maxEntityNameLen {
			return consolidationProposal{}, fmt.Errorf("консолидация: stance #%d: имя сущности длиннее %d символов", i, maxEntityNameLen)
		}
		sp.Aspect = strings.TrimSpace(sp.Aspect)
		if sp.Aspect == "" {
			sp.Aspect = "general"
		}
		if len([]rune(sp.Aspect)) > maxAspectLength {
			return consolidationProposal{}, fmt.Errorf("консолидация: stance #%d: аспект длиннее %d символов", i, maxAspectLength)
		}
		sp.Summary = strings.TrimSpace(sp.Summary)
		if sp.Summary == "" {
			return consolidationProposal{}, fmt.Errorf("консолидация: stance #%d без резюме", i)
		}
		sp.Strength = clamp01(sp.Strength)
		sp.Interest = clamp01(sp.Interest)
	}
	if len(p.EntityMerges) > maxEntityMerges {
		return consolidationProposal{}, fmt.Errorf("консолидация: больше %d предложений entity_merges", maxEntityMerges)
	}
	for i := range p.EntityMerges {
		mp := &p.EntityMerges[i]
		mp.Keep = strings.TrimSpace(mp.Keep)
		mp.Merge = strings.TrimSpace(mp.Merge)
		mp.Reason = strings.TrimSpace(mp.Reason)
		if mp.Keep == "" || mp.Merge == "" {
			return consolidationProposal{}, fmt.Errorf("консолидация: entity_merges #%d без keep/merge", i)
		}
		if len([]rune(mp.Keep)) > maxEntityNameLen || len([]rune(mp.Merge)) > maxEntityNameLen {
			return consolidationProposal{}, fmt.Errorf("консолидация: entity_merges #%d: имя сущности длиннее %d символов", i, maxEntityNameLen)
		}
		if mp.Reason == "" {
			return consolidationProposal{}, fmt.Errorf("консолидация: entity_merges #%d без основания", i)
		}
		if len([]rune(mp.Reason)) > maxMergeReasonLen {
			return consolidationProposal{}, fmt.Errorf("консолидация: entity_merges #%d: основание длиннее %d символов", i, maxMergeReasonLen)
		}
	}
	return p, nil
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// dumpTask — имя фоновой задачи для дампа модельных вызовов (40.1):
// operation без служебного префикса, «model:bg:intention:42:1» →
// bg-intention-42-1-call-N.json (modebug ещё санитизирует имя под файл).
func dumpTask(operation string) string {
	return strings.TrimPrefix(operation, "model:bg:")
}
