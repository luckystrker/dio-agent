// Ход (§11, §12): запуск ago-агента с восстановлением из журнала.
//
// Фазы (§12.1, §12.2):
//  0. чтение журнала хода: model_output есть — применение сохранённого
//     результата без новой генерации; deltas_applied есть — ход уже применён;
//  1. turn_started (однократно);
//  2. бюджет (§16) → контекст из БД (§11.1) → запуск агента ago: панель
//     состояния первой частью пачки, карточка памяти вместо принудительного
//     поиска (решение 2026-11-02), строгая валидация TurnResult (§11.2), один
//     repair-retry (§11.3), невалидный результат не применяется — терминальный
//     пропуск;
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
	"time"
	"unicode/utf8"

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

// now — часы Runner: тесты подменяют, иначе системное время.
func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// wantForcedTool — принуждение первого вызова памяти: только прямой ход и
// только первая попытка. Повторы идут без forced tool_choice: thinking-режимы
// части провайдеров с ним несовместимы, а повторная генерация уже аварийная
// (портировано из TS-линии c0a986d, 2026-10-31). С 2026-11-02 применяется
// только при MODEL_MEMORY_PREFETCH=false (откат): в обычном режиме память
// предзагружается карточкой, и forced-вызова нет (решение 2026-11-02).
func wantForcedTool(in Input, hasMemory bool) bool {
	return in.Direct && hasMemory && in.Attempt <= 1
}

// memoryToolsLimit — эффективный потолок обращений к памяти на разговорном
// ходе с предзагрузкой (решения 2026-11-02 и 2026-11-03): история целиком в
// окне модели, карточка несёт релевантное, инструменты — страховка на случай
// «в истории и карточке ответа нет». min(ToolCallsLimit, 2).
const memoryToolsLimit = 2

// memoryToolPlan — режим инструментов памяти на разговорном ходе. Enabled —
// выдавать инструменты; ForcedTool — принудительный первый вызов (только
// откат без предзагрузки); Limit — эффективный лимит обращений (0 — без
// ограничения).
type memoryToolPlan struct {
	Enabled    bool
	ForcedTool string
	Limit      int
}

// planMemoryTools решает, нужны ли инструменты памяти на этом ходе.
//
// Обычный режим (решения 2026-11-02 и 2026-11-03): история цела и в карточку
// влезло всё релевантное — модель уже видит весь разговор и нужные заметки,
// значит ход идёт одним вызовом БЕЗ инструментов памяти. История урезана или
// релевантное не влезло — инструменты включаются, но без forced-вызова и с
// лимитом min(ToolCallsLimit, 2): добрать недостающее, а не исследовать память
// заново.
//
// Откат (prefetch=false): прежнее поведение — инструменты всегда, forced на
// прямом обращении, лимит из конфигурации.
func planMemoryTools(in Input, bctx BaseContext, hasMemory, prefetch bool, configuredLimit int) memoryToolPlan {
	if !hasMemory {
		return memoryToolPlan{}
	}
	if !prefetch {
		plan := memoryToolPlan{Enabled: true, Limit: configuredLimit}
		if wantForcedTool(in, hasMemory) {
			plan.ForcedTool = memory.ToolSearch
		}
		return plan
	}
	if !bctx.HistoryTrimmed && bctx.MemoryCardFitted {
		return memoryToolPlan{}
	}
	limit := memoryToolsLimit
	if configuredLimit > 0 && configuredLimit < limit {
		limit = configuredLimit
	}
	return memoryToolPlan{Enabled: true, Limit: limit}
}

// memoryToolsReason — причина включения инструментов памяти в лог хода (21.4):
// history_trimmed / card_incomplete:<причина> / rollback / none. Причина
// неполноты карточки (28.3) уточняет, какая ветка сработала:
// card_incomplete:fts_cut|budget|stances|limit.
func memoryToolsReason(prefetch bool, bctx BaseContext) string {
	switch {
	case !prefetch:
		return "rollback"
	case bctx.HistoryTrimmed:
		return "history_trimmed"
	case !bctx.MemoryCardFitted:
		if bctx.MemoryCardIncomplete != "" {
			return "card_incomplete:" + bctx.MemoryCardIncomplete
		}
		return "card_incomplete"
	default:
		return "none"
	}
}

// ErrModelNotConfigured — модель не настроена: ход не выполняется и не
// помечается терминальным пропуском (конфигурация приходит — ход повторится).
var ErrModelNotConfigured = errors.New("модель не настроена (MODEL_ENDPOINT/API_KEY/MODEL)")

// Runner — исполнитель ходов. Один когнитивный worker (§5.2) вызывает Run
// последовательно; методы безопасны и при параллельном использовании.
type Runner struct {
	Pool    *runtime.Pool
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
	// MemoryPrefetch — память предзагружается карточкой в панель состояния
	// (решение 2026-11-02). false — откат к forced tool-calling.
	MemoryPrefetch bool
	// Timezone — пояс абсолютных меток истории (AGENT_TIMEZONE); nil — пояс
	// по умолчанию Europe/Moscow.
	Timezone *time.Location
	// Now — часы хода (тесты фиксируют время); nil — time.Now. Нужны цели
	// расписания дня при применении дельт (этап 22.2), как Assembler.Now.
	Now func() time.Time
	// Photos — скачивание фото Telegram (vision §6.1); nil — модель видит текст.
	Photos PhotoFetcher
	// Videos — скачивание видео (гифка/кружок) для раскадровки (этап 35);
	// nil — видео идёт превью-кадром.
	Videos VideoFetcher
	// Storyboard — внешний ffmpeg-каттер кадров (этап 35); nil — раскадровка
	// выключена (нет ffmpeg), остаётся превью-кадр.
	Storyboard StoryboardExtractor
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
	// Direct — прямое обращение (приоритет 1 §5.2). При откате
	// MODEL_MEMORY_PREFETCH=false первый вызов памяти принудительный (§10.4);
	// в обычном режиме память предзагружена карточкой (решение 2026-11-02).
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
	// Failed — терминальный провал (turn_failed, 29.1): попытки исчерпаны,
	// ход завершён ошибкой с причиной; повторно не генерируется.
	Failed     bool
	FailReason string
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
	if state.Failed {
		// Терминальный провал (29.1): попытки исчерпаны, результат «отравлен»
		// или применение не проходит. Новая генерация не запускается, даже
		// если в потоке остался model_output.
		out.Failed = true
		out.FailReason = state.FailReason
		r.logf("ход %d терминально провален ранее (%s)", in.TurnID, state.FailReason)
		return out, nil
	}
	if state.Skipped {
		out.Skipped = true
		out.SkipReason = state.SkipReason
		r.logf("ход %d терминально пропущен ранее (%s)", in.TurnID, state.SkipReason)
		return out, nil
	}
	if state.OutputInvalid {
		// Сохранённый model_output не проходит текущую валидацию (записан по
		// старым правилам или повреждён, 18.3): не паника и не тишина —
		// терминальный skip, система продолжает приём (инвариант 5).
		reason := fmt.Sprintf("recovery: сохранённый model_output не проходит валидацию: %s", state.OutputInvalidReason)
		r.logf("ERROR ход %d: %s", in.TurnID, reason)
		if err := r.Journal.WriteTurnSkipped(ctx, in.TurnID, "recovery", reason); err != nil {
			return out, fmt.Errorf("ход %d: turn_skipped не записан: %w", in.TurnID, err)
		}
		out.Skipped = true
		out.SkipReason = reason
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
	// Панель состояния — первая часть финального пользовательского сообщения,
	// перед пачкой (решение 2026-11-02): динамика не в system.
	plainUserText := composeUserText(bctx.StatePanel, userText)

	// Картинки пачки (vision §6.1, этап 25) и раскадровка гифок/кружков
	// (этап 35): фото, статичные стикеры, превью анимированных стикеров,
	// кадры видео — до вызова модели; ошибки — откат на текст/превью.
	visuals := CollectVisuals(ctx, in.Batch, r.Photos, r.Videos, r.Storyboard, telegram.MaxImagesPerTurn, func(format string, args ...any) {
		r.logf("ход %d: "+format, append([]any{in.TurnID}, args...)...)
	})
	images := visuals.Images
	if len(visuals.StoryboardFrames) > 0 {
		// Пометка «раскадровка N кадров» — в тексте события: модель должна
		// знать, что видит кадры видео, а не отдельные картинки.
		bctx.Batch = annotateStoryboards(bctx.Batch, in.Batch, visuals.StoryboardFrames)
		userText = strings.Join(bctx.Batch, "\n")
		if strings.TrimSpace(userText) == "" {
			userText = "[сообщение без текста]"
		}
	}
	userText = composeUserText(bctx.StatePanel, userText)
	if len(images) > 0 {
		r.logf("ход %d: картинок в запросе модели: %d (фото/стикеры/гифки/раскадровка)", in.TurnID, len(images))
	}
	result, repairs, llmCalls, reasoning, err := r.generate(ctx, in, bctx, userText, plainUserText, operation, images)
	out.LLMCalls = llmCalls
	var invalidErr *invalidResultError
	if errors.As(err, &invalidErr) {
		// Результат так и не стал валидным: терминальный пропуск с причиной
		// (§11.3: после исчерпания попыток — skipped, система продолжает приём).
		// 21.7: причина несёт сырые ответы модели обеих попыток — иначе
		// пропуск не разобрать по журналу.
		reason := invalidErr.skipReason()
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
	if err := r.Journal.WriteModelOutput(ctx, in.TurnID, result, nil, repairs,
		reasoning.First, reasoning.Repair); err != nil {
		return out, fmt.Errorf("ход %d: model_output не записан: %w", in.TurnID, err)
	}

	// --- Фаза 4: применение одной транзакцией ---
	return r.apply(ctx, in, result, out)
}

// invalidResultError — ответ модели не прошёл валидацию §11.2 ни с первой,
// ни с repair-попытки. Сырые тела обеих попыток едут в turn_skipped (21.7):
// без них терминальный пропуск не разобрать.
type invalidResultError struct {
	err    error
	first  string
	repair string
	// reasoning модели обеих попыток (26.3) — рядом с сырыми ответами:
	// пропуск без него не разобрать, когда ответ «почти правильный».
	firstReasoning  string
	repairReasoning string
}

func (e *invalidResultError) Error() string { return e.err.Error() }

// invalidOutputRunes — потолок сырого ответа модели в причине пропуска:
// журнал не должен раздуваться мегабайтами, но ответ обязан быть узнаваем.
const invalidOutputRunes = 2000

// skipReason — текст причины пропуска для журнала и лога: ошибка валидации +
// сырые ответы и reasoning первой и repair-попыток.
func (e *invalidResultError) skipReason() string {
	reason := fmt.Sprintf("валидация TurnResult не пройдена после repair: %v\nответ модели (попытка 1): %s\nответ на repair: %s",
		e.err, truncateRawOutput(e.first), truncateRawOutput(e.repair))
	if r := truncateReasoning(e.firstReasoning); r != "" {
		reason += "\nreasoning (попытка 1): " + r
	}
	if r := truncateReasoning(e.repairReasoning); r != "" {
		reason += "\nreasoning (repair): " + r
	}
	return reason
}

// truncateRawOutput режет сырой ответ модели по рунам (не байтам).
func truncateRawOutput(text string) string {
	if utf8.RuneCountInString(text) <= invalidOutputRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:invalidOutputRunes-1]) + "…"
}

// repairHint — подсказка repair-попытки (§11.3): общий шаблон + конкретика
// для известной ошибки «answer без сообщений» (21.7), иначе модель повторяет
// тот же пустой answer.
func repairHint(perr error) string {
	hint := RepairHint(perr, "TurnResult")
	if strings.Contains(perr.Error(), "answer требует") {
		hint += " decision=answer требует 1–4 сообщения; если текста нет — добавь sticker или reaction, иначе decision=react с эмодзи или ignore."
	}
	return hint
}

// turnReasoning — reasoning модели хода: первой попытки и repair (26.3).
// Пустые строки — провайдер reasoning не отдавал (поля в журнале нет).
type turnReasoning struct {
	First  string
	Repair string
}

// generate запускает ago-агента и добивается валидированного TurnResult
// (не более одного repair-retry, §11.3). Возвращает результат, число
// repair-попыток, число модельных вызовов и reasoning попыток.
func (r *Runner) generate(ctx context.Context, in Input, bctx BaseContext, userText, fallbackText, operation string, images []Photo) (TurnResult, int, int, turnReasoning, error) {
	// Свой конфиг на ход: все запуски агента внутри хода (первый Ask и
	// repair-продолжение через reply.Ask) делят один счётчик вызовов, поэтому
	// forced tool_choice встаёт ровно на первый вызов хода, а repair §11.3
	// получает обычный вызов и может ответить финальным TurnResult.
	cfg := &ChatConfig{params: r.Model.params, inner: r.Model.inner, SessionID: sessionForChat(in.ChatID)}
	plan := planMemoryTools(in, bctx, r.Memory != nil, r.MemoryPrefetch, r.ToolCallsLimit)
	r.logMemoryTurn(in, bctx, plan)
	// План инструментов памяти — durable событие журнала (этап 31.2):
	// метрики §20 читают причины из базы, а не парсят логи. Сбой записи не
	// роняет ход — журнал наблюдаемости, как у tool_call/tool_result.
	if err := r.Journal.WriteTurnPlan(ctx, in.TurnID, &TurnPlan{
		TurnID:      in.TurnID,
		Enabled:     plan.Enabled,
		Reason:      memoryToolsReason(r.MemoryPrefetch, bctx),
		CardEntries: bctx.MemoryCardEntries,
		CardFitted:  bctx.MemoryCardFitted,
	}); err != nil {
		r.logf("ход %d: turn_plan не записан: %v", in.TurnID, err)
	}
	if plan.ForcedTool != "" {
		// Forced tool-calling на прямом обращении (§10.4): только в режиме
		// отката (MODEL_MEMORY_PREFETCH=false). В обычном режиме память уже
		// в карточке панели (решение 2026-11-02).
		cfg.ForcedTool = plan.ForcedTool
	}

	// Middleware: бюджет §16 — внешний слой (резерв до всего остального);
	// журналирование tool_call/tool_result — наружу лимита, чтобы отклонённые
	// лимитом вызовы тоже были зафиксированы; typing §13.3 — на весь ход.
	budgetMW := NewBudgetMiddleware(r.Budget, operation, r.DailyCallsLimit, r.Logger)
	toolMW := NewToolLimitMiddleware(plan.Limit, r.Logger)
	// Исчерпание лимита отключает инструменты на конфиге хода: следующий
	// модельный вызов идёт без tools и без forced tool_choice (bug 2026-11-02).
	toolMW.SetOnExhausted(cfg.DisableTools)
	// Прямой ход на стоп-кране отвечает видимой реакцией, а не молчанием.
	toolMW.SetDirect(in.Direct)
	mws := []agomiddleware.Factory{func() agomiddleware.Middleware { return budgetMW }}
	// Reasoning (26.3) — на все модельные вызовы хода (включая tool-цикл и
	// repair): складывается отдельно от истории, только для журнала.
	reasoning := NewReasoningCapture()
	mws = append(mws, func() agomiddleware.Middleware { return reasoning })
	if plan.Enabled {
		mws = append(mws,
			func() agomiddleware.Middleware { return &toolJournalMW{runner: r, turnID: in.TurnID} },
			func() agomiddleware.Middleware { return toolMW },
		)
	}
	if r.Typing != nil {
		mws = append(mws, func() agomiddleware.Middleware { return NewTypingMiddleware(r.Typing, in.ChatID) })
	}

	// newAgent — сборка запуска агента: откат «провайдер не принял картинку»
	// строит НОВЫЙ запуск, иначе неудачный запрос с картинкой остался бы в
	// потоке и повторился бы в истории.
	newAgent := func() (*agent.Agent, error) {
		opts := []agent.Option{
			agent.WithConfig(cfg),
			// Системный промпт из шаблона §7.1; constraints — отдельная структурная
			// секция контекста (§17), не смешанная с данными пачки.
			agent.WithPromptFragments(bctx.Constraints),
			agent.WithResponseSchema(response.RawSchema(TurnResultJSONSchema(),
				response.WithRawName("TurnResult"),
				response.WithRawDescription("Цельный результат хода: решение, сообщения, кандидаты памяти, дельты состояния."))),
		}
		if plan.Enabled {
			opts = append(opts, agent.WithTools(r.Memory.Tools()...))
		}
		// История чата (§11.1 п.5): реплики прошлых ходов — настоящими user/
		// assistant сообщениями до пачки, append-only (кэш префикса провайдера
		// не страдает). Метки времени абсолютные, автор — из БД.
		if len(bctx.History) > 0 {
			seeded, err := historyStream(bctx.History, r.Timezone)
			if err != nil {
				return nil, fmt.Errorf("ход %d: история чата не посеяна: %w", in.TurnID, err)
			}
			opts = append(opts, agent.WithStreamBase(seeded))
		}
		opts = append(opts, agent.WithMiddleware(mws...))
		// DI аудитории хода: инструменты памяти фильтруют приватность до выдачи (§9.2).
		// Состояние поиска памяти — своё на ход: new_items/hint считают только
		// записи, уже выданные в этом ходе (bug 2026-11-02).
		opts = append(opts, agent.WithDependencies(map[string]any{
			memory.ScopeDependency:       in.ScopeID,
			memory.SearchStateDependency: memory.NewSearchState(),
		}))
		return agent.New("dio", bctx.System, opts...), nil
	}

	a, err := newAgent()
	if err != nil {
		return TurnResult{}, 0, 0, turnReasoning{}, err
	}
	// Части Ask: текст пачки + картинки BinaryInput (находка приёмки Р: фото
	// скачивалось, но в вызов модели уходил только текст; этап 25 добавил
	// стикеры и превью гифок).
	reply, err := a.Ask(ctx, askMsgs(userText, images)...)
	if err != nil && len(images) > 0 && looksLikeMediaRejection(err) {
		// Honest fallback (25.1): провайдер не принял картинку (например,
		// image/webp) — повтор без картинок, стикеры/гифки остаются текстом.
		// Молча терять ответ нельзя, поэтому причина — в лог.
		r.logf("ход %d: провайдер не принял картинку (%v) — повтор без картинок, стикеры/гифки/раскадровка остаются текстом", in.TurnID, err)
		if a, err = newAgent(); err != nil {
			return TurnResult{}, 0, budgetMW.callCount(), turnReasoning{}, err
		}
		// Без картинок пометка раскадровки была бы ложью — берём текст до неё.
		reply, err = a.Ask(ctx, fallbackText)
	}
	if err != nil {
		return TurnResult{}, 0, budgetMW.callCount(), turnReasoning{}, err
	}
	// Reasoning первой попытки (весь tool-цикл Ask) — до repair.
	reasoningOut := turnReasoning{First: reasoning.Take()}

	// --- строгая валидация ответа (§11.2); один repair-retry (§11.3) ---
	repairs := 0
	result, body, perr := parseReply(reply, bctx.History)
	if perr != nil {
		repairs = 1
		// Первый невалидный ответ — в журнал даже при удачном repair: иначе
		// ход выглядит здоровым, а причина лишнего вызова модели исчезает.
		r.logf("ход %d: первый ответ не прошёл валидацию (%v) — repair; тело (%d рун): %s",
			in.TurnID, perr, utf8.RuneCountInString(body), truncateRawOutput(body))
		fixed, ferr := reply.Ask(ctx, repairHint(perr))
		if ferr != nil {
			return TurnResult{}, repairs, budgetMW.callCount(), reasoningOut, ferr
		}
		reasoningOut.Repair = reasoning.Take()
		var repairBody string
		result, repairBody, perr = parseReply(fixed, bctx.History)
		if perr != nil {
			return TurnResult{}, repairs, budgetMW.callCount(), reasoningOut, &invalidResultError{
				err: perr, first: body, repair: repairBody,
				firstReasoning: reasoningOut.First, repairReasoning: reasoningOut.Repair,
			}
		}
	}
	return result, repairs, budgetMW.callCount(), reasoningOut, nil
}

// parseReply разбирает тело ответа как строгий TurnResult (§11.2); история
// нужна, чтобы срезать скопированного автора реплики до валидации. Сырое тело
// возвращается для turn_skipped (21.7).
func parseReply(reply *agent.AgentReply, history []HistoryMessage) (TurnResult, string, error) {
	body, err := reply.Content()
	if err != nil {
		return TurnResult{}, "", err
	}
	result, perr := ParseTurnResultWithHistory(body, history)
	return result, string(body), perr
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
		TurnID:  in.TurnID,
		ChatID:  in.ChatID,
		ScopeID: in.ScopeID,
		Result:  result,
		// Цель затухания — блок расписания дня (этап 22.2): дельта ложится на
		// «сегодняшнюю» базу, а не на статичную линию персоны.
		PersonaBaseline: r.Persona.MoodTargetAt(r.now(), r.Timezone),
		Journal:         r.Journal,
		// Разговорный ход сбрасывает скуку (этап 23.1).
		BoredomReset: true,
		Now:          r.now(),
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
	} else {
		// Аватарка собеседника (34.2): ставим фоновую проверку после
		// применения — модель узнает об аватарке со следующего хода, ответ уже
		// не задержан.
		enqueueAvatarCheck(ctx, r.Pool, in, r.logf)
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
	// Стикеры (этап 25.4–25.5): мягко обнулённая метка и сохранение — в лог,
	// иначе «модель просила стикер, а его нет» не отличить от «не просила».
	if out.Apply.SkippedSticker {
		r.logf("ход %d: метка стикера %q не найдена в коллекции — стикер не отправлен", in.TurnID, result.Sticker)
	}
	if out.Apply.SuppressedSticker {
		r.logf("ход %d: стикер %q мягко обнулён предохранителем частоты — в последних %d ходах чата стикер уже уходил (28.1)",
			in.TurnID, result.Sticker, stickerRateWindow)
	}
	if out.Apply.SavedStickerID != 0 {
		r.logf("ход %d: стикер из пачки сохранён в коллекцию (#%d)", in.TurnID, out.Apply.SavedStickerID)
	}
	if out.Apply.DroppedStickerSave {
		r.logf("ход %d: save_sticker отброшен — в пачке нет стикера, которого не было бы в коллекции (28.1)", in.TurnID)
	}
	// Дубли намерений (§14.1, блок 17.5): причина отклонения предложения —
	// в лог оператора, молчаливая потеря не видна ни в чате, ни в журнале.
	for _, dup := range out.Apply.DuplicateIntentions {
		r.logf("ход %d: намерение %s %q не создано — живое намерение с тем же dedupe-ключом уже есть (статус %s, §14.1)",
			in.TurnID, dup.Kind, dup.Topic, dup.LiveStatus)
	}
	// Страховки намерений (26.2): поднятая задержка и отсеянная мусорная тема
	// тоже видны оператору — иначе «модель попросила через 5 минут, а пришло
	// через 2 часа» не отличить от молчаливой правки.
	for _, raised := range out.Apply.RaisedIntentions {
		r.logf("ход %d: намерение %s %q: delay_hours %.3f поднята до %.3f ч (runtime-пол вида, 26.2)",
			in.TurnID, raised.Kind, raised.Topic, raised.From, raised.To)
	}
	for _, rejected := range out.Apply.RejectedIntentions {
		r.logf("ход %d: намерение %s %q отклонено — тема-мусор (%s, §14.1)",
			in.TurnID, rejected.Kind, rejected.Topic, rejected.Reason)
	}
	for _, limited := range out.Apply.LimitedIntentions {
		r.logf("ход %d: лимит инициатив исчерпан — намерение не создано (%s %q, §14.3)",
			in.TurnID, limited.Kind, limited.Topic)
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

// logMemoryTurn — одна строка наблюдаемости разговорного хода (21.4):
// инструменты вкл/выкл и причина, объём карточки и её полнота, семантика
// памяти. Без неё «модель опять сожгла лимит» не отличить от «память
// предзагружена, но не влезла».
func (r *Runner) logMemoryTurn(in Input, bctx BaseContext, plan memoryToolPlan) {
	state := "выкл"
	if plan.Enabled {
		state = "вкл"
	}
	semantic := "выключена"
	if model := r.Memory.SemanticModel(); model != "" {
		semantic = "включена (" + model + ")"
	}
	r.logf("ход %d: память — инструменты %s (причина: %s), карточка: записей=%d fits=%v, семантика: %s",
		in.TurnID, state, memoryToolsReason(r.MemoryPrefetch, bctx), bctx.MemoryCardEntries, bctx.MemoryCardFitted, semantic)
}
