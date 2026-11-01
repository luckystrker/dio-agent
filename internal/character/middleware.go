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
	allowed, err := m.store.Reserve(context.Background(), operationID, attemptOf(m.operation), m.pool, 1, m.limit)
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
			if rerr := m.store.Release(context.Background(), operationID, m.pool, 1); rerr != nil {
				m.logger.Printf("бюджет: резерв %s не возвращён после отмены хода: %v", operationID, rerr)
			}
			return nil, err
		}
		// Вызов не завершился: стоимость неизвестна, actual не пишется нулевым (§16).
		_ = m.store.RecordActual(context.Background(), operationID, m.pool, 0, 1, true)
		return nil, err
	}
	tokens := int64(0)
	if resp != nil {
		if resp.Usage.PromptTokens != nil {
			tokens += *resp.Usage.PromptTokens
		}
		if resp.Usage.CompletionTokens != nil {
			tokens += *resp.Usage.CompletionTokens
		}
	}
	if err := m.store.RecordActual(context.Background(), operationID, m.pool, tokens, 1, false); err != nil {
		m.logger.Printf("бюджет: фактический usage не записан: %v", err)
	}
	return resp, nil
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

// ToolLimitMiddleware — ограничение числа обращений к инструментам за ход
// (§11.2: лимит конфигурируемый). Превышение отвечает модели ошибкой вместо
// исполнения: модель видит отказ и обязана завершить ход финальным ответом.
// Если модель отказ игнорирует, срабатывает стоп-кран (16.5): после
// stopCraneRejects подряд отказов middleware возвращает финальный результат
// инструмента (ToolResult с Final=true), который executor ago обращает в
// ModelResponse с ResponseForce — агентский цикл завершается без нового
// модельного вызова (живой смоук: серия отказов = серия платных вызовов).
type ToolLimitMiddleware struct {
	middleware.Base
	limit  int
	logger *log.Logger

	mu sync.Mutex
	// seen — счётчик всех обращений за ход; rejects — текущая серия подряд
	// идущих отказов лимита (обращение в лимите серию обнуляет).
	seen    int
	rejects int
}

// stopCraneRejects — сколько подряд отказов лимита замыкают ход принудительно.
const stopCraneRejects = 3

// NewToolLimitMiddleware строит middleware лимита обращений (0 = не ограничивать).
func NewToolLimitMiddleware(limit int, logger *log.Logger) *ToolLimitMiddleware {
	if logger == nil {
		logger = discardLogger()
	}
	return &ToolLimitMiddleware{limit: limit, logger: logger}
}

// OnToolExecution считает обращения и гасит лишние.
func (m *ToolLimitMiddleware) OnToolExecution(next middleware.ToolExecFunc, ev *events.ToolCallEvent, cx *agocontext.Context) (events.Event, error) {
	m.mu.Lock()
	m.seen++
	over := m.limit > 0 && m.seen > m.limit
	if over {
		m.rejects++
	} else {
		m.rejects = 0
	}
	crane := over && m.rejects >= stopCraneRejects
	m.mu.Unlock()

	if crane {
		m.logger.Printf("стоп-кран: %d подряд отказов лимита обращений (%d) — ход завершается принудительно", m.rejects, m.limit)
		// Финальный результат с телом валидного TurnResult (ignore): цикл
		// инструментов закрывается ResponseForce без repair-вызова модели,
		// ход применяется молча. Тело собирается из того же контракта §11.2,
		// а не строкой — расхождение со схемой исключено.
		body, err := json.Marshal(TurnResult{
			Decision: DecisionIgnore,
			Reason:   fmt.Sprintf("стоп-кран: %d подряд отказа лимита обращений к инструментам — ход завершён без ответа модели", stopCraneRejects),
		})
		if err != nil {
			// Недостижимо (плоская структура), но финал не имеет права упасть.
			m.logger.Printf("стоп-кран: результат не собран: %v", err)
			return &events.ToolErrorEvent{
				ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
				Error:           "лимит обращений к инструментам за ход исчерпан — завершай ход финальным ответом",
			}, nil
		}
		return &events.ToolResultEvent{
			ParentID: ev.ID,
			Name:     ev.Name,
			Result:   events.NewToolResult(true, string(body)),
		}, nil
	}
	if over {
		m.logger.Printf("лимит обращений к инструментам исчерпан (%d) — вызов %q отклонён", m.limit, ev.Name)
		return &events.ToolErrorEvent{
			ToolResultEvent: events.ToolResultEvent{ParentID: ev.ID, Name: ev.Name},
			Error:           "лимит обращений к инструментам за ход исчерпан — завершай ход финальным ответом",
		}, nil
	}
	return next(ev, cx)
}
