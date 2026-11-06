// Middleware хода (ago/middleware): бюджет §16, typing-пульс §13.3 и лимит
// обращений к инструментам §11.2 со стоп-краном серии отказов (16.5).
//
// Бюджет: middleware не пропускает модельный вызов без резерва (§16) — check и
// reserve выполняются до next(), исчерпание дневного лимита завершает ход
// ошибкой (восстанавливаемой: turn_started без model_output → повтор по §12.2).
//
// Typing: «печатает…» показывается, пока реально ожидается ответ хода — старт
// сразу при открытии хода, обновление примерно раз в 4 секунды, stop в finally
// при завершении/отмене/таймауте (§13.3). Пульс не имеет права ронять ход.
package character

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/middleware"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// discardLogger — logger по умолчанию для middleware в тестах: пишет в
// io.Discard (log.New(nil, …) паниковал бы на первой записи).
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// BudgetMiddleware — резервирование модельного вызова до вызова модели (§16).
// operationID детерминирован: <prefix>:<call> где prefix = model:turn:<id>:<attempt>
// — повтор хода той же попытки не расходует лимит дважды, новая попытка — новый
// attempt ID (§16).
type BudgetMiddleware struct {
	middleware.Base
	store     *runtime.BudgetStore
	pool      string
	operation string // префикс операции без номера вызова
	limit     int64
	logger    *log.Logger

	mu        sync.Mutex
	callIndex int
}

// NewBudgetMiddleware строит middleware бюджета. operation — префикс вида
// "model:turn:42:0"; лимит 0 = не ограничивать; пул — conversation (§16).
func NewBudgetMiddleware(store *runtime.BudgetStore, operation string, limit int64, logger *log.Logger) *BudgetMiddleware {
	return NewBudgetMiddlewarePool(store, operation, limit, runtime.BudgetPoolConversation, logger)
}

// NewBudgetMiddlewarePool — то же с явным пулом бюджета (§16: conversation |
// background | maintenance). Нужен фону (9.6): исследование не должно
// расходовать бюджет, оставленный на прямые разговоры.
func NewBudgetMiddlewarePool(store *runtime.BudgetStore, operation string, limit int64, pool string, logger *log.Logger) *BudgetMiddleware {
	if logger == nil {
		logger = discardLogger()
	}
	return &BudgetMiddleware{
		store:     store,
		pool:      pool,
		operation: operation,
		limit:     limit,
		logger:    logger,
	}
}

// OnLLMCall — check+reserve до модельного вызова, фактический usage после.
func (m *BudgetMiddleware) OnLLMCall(next middleware.LLMCallFunc, evs []events.Event, cx *agocontext.Context) (*events.ModelResponse, error) {
	m.mu.Lock()
	idx := m.callIndex
	m.callIndex++
	m.mu.Unlock()

	operationID := fmt.Sprintf("%s:%d", m.operation, idx)
	qctx, qcancel := context.WithTimeout(context.Background(), budgetDBTimeout)
	allowed, err := m.store.Reserve(qctx, operationID, attemptOf(m.operation), m.pool, 1, m.limit)
	qcancel()
	if err != nil {
		if errors.Is(err, runtime.ErrBudgetExhausted) {
			m.logger.Printf("ход прерван: %v", err)
			return nil, err
		}
		// Сбой резерва (БД недоступна) тоже не пропускает вызов: без резерва
		// модель не вызывается (§16).
		return nil, fmt.Errorf("бюджет: резерв не выполнен: %w", err)
	}
	_ = allowed // true: и новый резерв, и уже оплаченный retry проходят дальше

	resp, err := next(evs, cx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// Ход отменён — модельный вызов не состоялся: резерв возвращается
			// (§16, блок 17.3), день не выжигается храповиком. Отмена ход
			// остаётся в очереди (§12.2), повтор получит свой attempt ID.
			if rerr := m.releaseBudget(operationID); rerr != nil {
				m.logger.Printf("бюджет: резерв %s не возвращён после отмены хода: %v", operationID, rerr)
			}
			return nil, err
		}
		// Вызов не завершился: стоимость неизвестна, actual не пишется нулевым (§16).
		rctx, rcancel := context.WithTimeout(context.Background(), budgetDBTimeout)
		_ = m.store.RecordActual(rctx, operationID, m.pool, 0, 1, true)
		rcancel()
		return nil, err
	}
	tokens, prompt, cached := usageTokens(resp)
	if idx == 0 {
		// Наблюдаемость кэша префикса (39.2): cached_tokens первого вызова
		// хода. cached=«нет данных» — провайдер не сообщил (не ложный промах).
		cacheText := "нет данных"
		if cached != nil {
			cacheText = fmt.Sprintf("%d", *cached)
		}
		m.logger.Printf("модель: первый вызов хода — prompt_tokens=%d cached_tokens=%s", prompt, cacheText)
	}
	rctx, rcancel := context.WithTimeout(context.Background(), budgetDBTimeout)
	err = m.store.RecordActualUsage(rctx, operationID, m.pool,
		runtime.ActualUsage{Tokens: tokens, Calls: 1, CachedTokens: cached, PromptTokens: promptOrNil(resp)})
	rcancel()
	if err != nil {
		m.logger.Printf("бюджет: фактический usage не записан: %v", err)
	}
	return resp, nil
}

// budgetDBTimeout — потолок служебной записи бюджета: middleware не получает
// context хода, но зависший acquire не должен держать модельный вызов
// бесконечно (minor 2).
const budgetDBTimeout = 5 * time.Second

// releaseBudget возвращает резерв с ограниченным отдельным контекстом.
func (m *BudgetMiddleware) releaseBudget(operationID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), budgetDBTimeout)
	defer cancel()
	return m.store.Release(ctx, operationID, m.pool, 1)
}

// usageTokens — токены ответа, промпта и кэшированное попадание префикса
// (решение 2026-11-02): ago отдаёт CacheReadInputTokens
// (prompt_tokens_details.cached_tokens у OpenAI-совместимых провайдеров).
// Провайдер не сообщил — nil, а не 0: «неизвестно» не должно выглядеть как
// полный промах кэша.
func usageTokens(resp *events.ModelResponse) (tokens, prompt int64, cached *int64) {
	if resp == nil {
		return 0, 0, nil
	}
	if resp.Usage.PromptTokens != nil {
		prompt = *resp.Usage.PromptTokens
		tokens += prompt
	}
	if resp.Usage.CompletionTokens != nil {
		tokens += *resp.Usage.CompletionTokens
	}
	return tokens, prompt, resp.Usage.CacheReadInputTokens
}

// promptOrNil — prompt_tokens ответа для записи в actual: провайдер не
// сообщил — nil (доля кэша по такой строке не считается).
func promptOrNil(resp *events.ModelResponse) *int64 {
	if resp == nil {
		return nil
	}
	return resp.Usage.PromptTokens
}

// callCount — сколько модельных вызовов сделано в рамках хода (§20).
func (m *BudgetMiddleware) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callIndex
}

// attemptOf вытаскивает номер попытки из суффикса префикса операции
// "model:turn:<id>:<attempt>". Нечисловое — попытка 0.
func attemptOf(operation string) int {
	for i := len(operation) - 1; i >= 0; i-- {
		if operation[i] == ':' {
			n := 0
			for _, c := range operation[i+1:] {
				if c < '0' || c > '9' {
					return 0
				}
				n = n*10 + int(c-'0')
			}
			return n
		}
	}
	return 0
}

// ChatActionSender — отправка chat action (реализует Telegram-адаптер 9.5).
type ChatActionSender interface {
	SendChatAction(ctx context.Context, chatID int64, action string) error
}

// TypingPulse — «печатает…» по чату: старт сразу, обновление ~раз в 4 с,
// stop прекращает. Ошибки api глотаются: пульс не роняет ход (8.4, §13.3).
type TypingPulse struct {
	send     ChatActionSender
	interval time.Duration

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
}

// NewTypingPulse создаёт пульс над отправителем chat action; интервал 0 —
// 4 секунды (§13.3).
func NewTypingPulse(send ChatActionSender, interval time.Duration) *TypingPulse {
	if interval <= 0 {
		interval = 4 * time.Second
	}
	return &TypingPulse{send: send, interval: interval, cancels: map[int64]context.CancelFunc{}}
}

// Start запускает пульс чата: первое действие сразу, затем по тикеру.
func (p *TypingPulse) Start(chatID int64) {
	p.Stop(chatID) // повторный старт заменяет прежний пульс (паритет TS)
	if p.send == nil {
		return // отправитель не настроен (telegram выключен) — пульсу нечем тикать (16.2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancels[chatID] = cancel
	p.mu.Unlock()

	go func() {
		defer cancel()
		fire := func() {
			// Паника адаптера глотается с записью в лог: «печатает…» не имеет
			// права ронять ход и процесс (16.2). Стандартный логгер — у пульса
			// нет своего, а строчка должна попасть в журнал процесса (systemd).
			defer func() {
				if r := recover(); r != nil {
					log.Printf("typing: пульс чата %d подавил панику отправителя: %v", chatID, r)
				}
			}()
			_ = p.send.SendChatAction(ctx, chatID, "typing") // ошибка глотается
		}
		fire()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fire()
			}
		}
	}()
}

// Stop прекращает пульс чата (завершение, отмена, timeout — §13.3).
func (p *TypingPulse) Stop(chatID int64) {
	p.mu.Lock()
	cancel, ok := p.cancels[chatID]
	delete(p.cancels, chatID)
	p.mu.Unlock()
	if ok {
		cancel()
	}
}

// TypingMiddleware — ago-middleware хода: пульс на весь ход, stop в finally.
type TypingMiddleware struct {
	middleware.Base
	pulse  *TypingPulse
	chatID int64
}

// NewTypingMiddleware оборачивает ход пульсом «печатает…».
func NewTypingMiddleware(pulse *TypingPulse, chatID int64) *TypingMiddleware {
	return &TypingMiddleware{pulse: pulse, chatID: chatID}
}

// OnTurn — старт сразу, stop в finally при любом исходе (§13.3).
func (m *TypingMiddleware) OnTurn(next middleware.TurnFunc, ev *events.ModelRequest, cx *agocontext.Context) (*events.ModelResponse, error) {
	m.pulse.Start(m.chatID)
	defer m.pulse.Stop(m.chatID)
	return next(ev, cx)
}

// ToolKindCap — потолок одного вида инструментов за ход (38.4): память ≤2,
// поиск ≤2, страницы ≤2. Отказ вида не выключает остальные инструменты: модель
// может завершить ответ другим видом, а не потерять ход.
type ToolKindCap struct {
	Kind   string   // memory | search | fetch (имя вида — для карты счётчиков)
	Limit  int      // потолок обращений этого вида; 0 — не ограничивать
	Names  []string // имена инструментов вида
	Reject string   // текст tool-error модели
}

// ToolLimitMiddleware — ограничение числа обращений к инструментам за ход
// (§11.2: лимит конфигурируемый). Превышение отвечает модели ошибкой вместо
// исполнения. С этапа 39 первый отказ ЛЮБОГО вида (или исчерпание общего
// лимита) закрывает инструменты на ходе: следующий модельный вызов идёт с
// tool_choice=none (определения остаются объявлены — 39.2), а текст отказа
// прямо говорит «ответь текстом сейчас». Модель обязана завершить ход
// финальным TurnResult.
//
// Если модель игнорирует и tool_choice=none (снова зовёт инструмент при уже
// закрытых инструментах), срабатывает стоп-кран: middleware возвращает
// финальный результат (ToolResult с Final=true), executor ago обращает его в
// ModelResponse с ResponseForce — агентский цикл завершается без нового
// модельного вызова. Тело крана: на прямом ходе — react «🤔», на
// непрямом/фоновом — ignore (39.1: видимая реакция — только когда финальный
// вызов без инструментов не дал валидного TurnResult).
type ToolLimitMiddleware struct {
	middleware.Base
	limit  int
	logger *log.Logger

	mu sync.Mutex
	// seen — счётчик всех обращений за ход; rejects — текущая серия подряд
	// идущих отказов лимита (обращение в лимите серию обнуляет).
	seen    int
	rejects int
	// kindCaps — раздельные потолки видов (38.4); kindSeen — счётчики по видам.
	kindCaps []ToolKindCap
	kindSeen map[string]int
	// onExhausted — колбэк харнесса на первый отказ: он закрывает инструменты
	// на конфиге хода (tool_choice=none), чтобы следующий вызов модели был
	// финальным.
	onExhausted func()
	// toolsClosed — инструменты уже закрыты на этом ходе (харнессом при
	// tool_choice=none или первым отказом лимита).
	toolsClosed bool
	// warned — модель уже получила подсказку «инструменты закрыты, ответь
	// текстом»; повторный вызов после неё замыкает ход краном.
	warned bool
	// closedByReject — инструменты закрыты именно отказом лимита (39.1):
	// только тогда невалидный финальный ответ деградирует в react, а не в
	// терминальный пропуск.
	closedByReject bool
	// direct — прямой ход (§5.2 приоритет 1): стоп-кран отдаёт видимую
	// реакцию вместо молчаливого ignore.
	direct bool
}

// stopCraneRejects — сколько подряд отказов лимита замыкают ход принудительно
// после того, как инструменты уже закрыты (модель игнорирует tool_choice=none).
const stopCraneRejects = 1

// NewToolLimitMiddleware строит middleware лимита обращений (0 = не ограничивать).
func NewToolLimitMiddleware(limit int, logger *log.Logger) *ToolLimitMiddleware {
	if logger == nil {
		logger = discardLogger()
	}
	return &ToolLimitMiddleware{limit: limit, logger: logger}
}

// SetOnExhausted подписывает харнесс на первый отказ лимита: он закрывает
// инструменты на конфиге хода (tool_choice=none). Вызывается до запуска агента.
func (m *ToolLimitMiddleware) SetOnExhausted(fn func()) {
	m.mu.Lock()
	m.onExhausted = fn
	m.mu.Unlock()
}

// SetClosed отмечает, что харнесс уже закрыл инструменты на этом ходе
// (tool_choice=none: память не нужна и веб исчерпан, 39.2). Вызывается до
// запуска агента: вызов инструмента такой моделью — нарушение, и middleware
// ответит подсказкой, а повторный вызов замкнёт ход.
func (m *ToolLimitMiddleware) SetClosed() {
	m.mu.Lock()
	m.toolsClosed = true
	m.mu.Unlock()
}

// SetDirect отмечает прямой ход: стоп-кран ответит видимой реакцией.
func (m *ToolLimitMiddleware) SetDirect(direct bool) {
	m.mu.Lock()
	m.direct = direct
	m.mu.Unlock()
}

// SetKindCaps задаёт раздельные потолки видов (38.4). Вызывается до запуска
// агента. Отказ вида считается первым отказом хода (39.1) и тоже закрывает
// инструменты: после него следующий вызов модели обязан дать текст.
func (m *ToolLimitMiddleware) SetKindCaps(caps []ToolKindCap) {
	m.mu.Lock()
	m.kindCaps = caps
	if m.kindSeen == nil {
		m.kindSeen = make(map[string]int, len(caps))
	}
	m.mu.Unlock()
}

// kindCapOver считает обращение вида и возвращает потолок, если он превышен
// (вызывается под m.mu).
func (m *ToolLimitMiddleware) kindCapOver(name string) *ToolKindCap {
	for i := range m.kindCaps {
		cap := &m.kindCaps[i]
		for _, toolName := range cap.Names {
			if toolName != name {
				continue
			}
			m.kindSeen[cap.Kind]++
			if cap.Limit > 0 && m.kindSeen[cap.Kind] > cap.Limit {
				return cap
			}
			return nil
		}
	}
	return nil
}

// OnToolExecution считает обращения и гасит лишние.
func (m *ToolLimitMiddleware) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	m.mu.Lock()
	m.seen++
	overKind := m.kindCapOver(ev.Name)
	over := m.limit > 0 && m.seen > m.limit
	// После закрытия инструментов любой вызов отклоняется: tool_choice=none уже
	// сказал модели «ответь текстом», а она снова зовёт инструмент.
	rejected := overKind != nil || over || m.toolsClosed
	// Кран — только когда модель уже получила подсказку о закрытии и всё равно
	// зовёт инструмент (игнорирует tool_choice=none).
	warnedBefore := m.warned
	closeNow := false
	if rejected {
		m.rejects++
		switch {
		case !m.toolsClosed:
			// Первый отказ ЛЮБОГО вида (39.1): закрываем инструменты и
			// возвращаем модели подсказку «ответь текстом сейчас».
			m.toolsClosed = true
			m.closedByReject = true
			m.warned = true
			closeNow = true
		case !m.warned:
			// Инструменты были закрыты харнессом с самого начала (39.2):
			// первый вызов — нарушение, но сначала даём подсказку.
			m.closedByReject = true
			m.warned = true
		}
	} else {
		m.rejects = 0
	}
	// Кран — повторный вызов после уже выданной подсказки о закрытии.
	crane := rejected && m.toolsClosed && warnedBefore && m.rejects >= stopCraneRejects
	direct := m.direct
	onExhausted := m.onExhausted
	rejects := m.rejects
	rejectText := "лимит обращений к инструментам за ход исчерпан — инструменты на этот ход закрыты, ответь текстом сейчас"
	if overKind != nil {
		rejectText = overKind.Reject + " — инструменты на этот ход закрыты, ответь текстом сейчас"
	}
	m.mu.Unlock()

	// Первый отказ ЛЮБОГО вида (39.1): закрываем инструменты — следующий вызов
	// модели идёт с tool_choice=none (определения остаются, 39.2) и обязан
	// выдать финальный TurnResult.
	if closeNow && onExhausted != nil {
		onExhausted()
	}

	if crane {
		m.logger.Printf("стоп-кран: %d подряд отказов лимита обращений (%d) после закрытия инструментов — ход завершается принудительно", rejects, m.limit)
		// Финальный результат с телом валидного TurnResult: цикл инструментов
		// закрывается ResponseForce без нового модельного вызова. Тело крана —
		// резервный путь: финальный вызов с tool_choice=none не дал текста
		// (модель снова позвала инструмент). На прямом ходе — react «🤔»,
		// на непрямом/фоновом — ignore.
		result := craneFallbackResult(direct)
		body, err := json.Marshal(result)
		if err != nil {
			// Недостижимо (плоская структура), но финал не имеет права упасть.
			m.logger.Printf("стоп-кран: результат не собран: %v", err)
			return &events.ToolErrorEvent{
				ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
				Error:           rejectText,
			}, nil
		}
		return &events.ToolResultEvent{
			ParentID: ev.ID,
			Name:     ev.Name,
			Result:   events.NewToolResult(true, string(body)),
		}, nil
	}
	if rejected {
		m.logger.Printf("лимит обращений к инструментам исчерпан (%d) — вызов %q отклонён: %s", m.limit, ev.Name, rejectText)
		return &events.ToolErrorEvent{
			ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
			Error:           rejectText,
		}, nil
	}
	return next(ev, cx)
}

// toolsClosedNow — инструменты закрыты middleware из-за отказа лимита (39.1):
// харнесс использует это, чтобы отличить стоп-кран от хода, где инструменты не
// были нужны с самого начала (там невалидный ответ остаётся терминальным
// пропуском, а не превращается в реакцию).
func (m *ToolLimitMiddleware) toolsClosedNow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closedByReject
}
