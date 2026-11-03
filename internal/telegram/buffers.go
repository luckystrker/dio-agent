// Буферы чатов (§6.2): собирают сообщения в порцию, пока пользователь печатает.
// Окна из спецификации; закрытие — по idle (тишина), по max (от первого
// события), по 20 событиям или лимиту текста. Таймер берёт ближайший из
// idle/max сроков (два таймера — срабатывает первый), поток сообщений не может
// бесконечно переносить закрытие — max идёт от первого события. Никакого
// ожидания внутри ingest-пути: только таймеры.
//
// In-memory таймер можно потерять; принадлежность событий созданному turn —
// нельзя: закрытие порции записывается транзакционно (§6.2). Поведение —
// src/telegram/buffers.ts.
package telegram

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// Windows — окно буфера одного сценария (§6.2).
type Windows struct {
	Idle time.Duration // тишина, после которой порция закрывается
	Max  time.Duration // максимум от первого события буфера
}

// DefaultWindows — стартовые окна §6.2 (настраиваемые значения, не гарантии).
func DefaultWindows(chatType ChatType, kind EventKind) Windows {
	if chatType == ChatPrivate {
		// Личка: idle 3 с (решение 2026-11-01, было 1.5 с — бурст человека
		// с паузами в пару секунд рвался на отдельные ходы, бот отвечал
		// на каждое сообщение по отдельности), максимум 10 с от первого
		// события — непрерывно печатающий не ждёт дольше этого.
		return Windows{Idle: 3 * time.Second, Max: 10 * time.Second}
	}
	switch kind {
	case KindReply, KindMention, KindCommand:
		// Reply / mention / command: 0–300 мс idle, максимум 1 с.
		return Windows{Idle: 150 * time.Millisecond, Max: time.Second}
	default:
		// Пассивная группа: 3–5 с idle, максимум 6 с.
		return Windows{Idle: 4 * time.Second, Max: 6 * time.Second}
	}
}

// Стартовые лимиты закрытия по объёму (§6.2).
const (
	MaxEventsPerBatch = 20
	// MaxBatchTextChars — лимит входного текста порции в символах (рунах),
	// паритет .length TS (решение 9.4: length-лимиты в символах).
	MaxBatchTextChars = 8000
)

// PendingEvent — новое pending-событие для буфера (от poller после ingest).
type PendingEvent struct {
	EventID  int64
	ChatID   int64
	ChatType ChatType
	Kind     EventKind
	Text     *string
}

// StartTurnFunc — транзакционное закрытие порции: создаёт turn с постоянным
// ID и привязывает события (§6.2). kind — «direct» | «group» (приоритеты §5.2).
type StartTurnFunc func(ctx context.Context, chatID int64, eventIDs []int64) (turnID int64, kind string, err error)

// BufferDeps — зависимости менеджера буферов.
type BufferDeps struct {
	Pool *runtime.Pool
	// Logger; nil — log.Discard.
	Logger *log.Logger
	// OnTurnReady — уведомление о созданном turn (точка подключения
	// когнитивного worker §5.2); nil — не вызывается.
	OnTurnReady func(turnID, chatID int64, kind string)
}

// ChatBufferManager — сборка порций по чатам.
type ChatBufferManager struct {
	deps   BufferDeps
	logger *log.Logger

	// Windows — окна по сценариям (по умолчанию DefaultWindows §6.2);
	// тесты подставляют короткие.
	Windows func(ChatType, EventKind) Windows
	// MaxEvents/MaxTextChars — лимиты закрытия; тесты уменьшают.
	MaxEvents    int
	MaxTextChars int

	// startTurn — реализация закрытия (по умолчанию dbStartTurn).
	startTurn StartTurnFunc

	mu      sync.Mutex
	buffers map[int64]*bufferState
	wg      sync.WaitGroup // незакрытые createTurn для FlushAll/Wait
	// turnMu сериализует создание ходов: два closeAsync одного чата не должны
	// получить id в порядке, обратном порядку событий (находка ревью).
	turnMu sync.Mutex
}

type bufferState struct {
	chatID    int64
	eventIDs  []int64
	textChars int
	windows   Windows
	idle      *time.Timer
	max       *time.Timer
}

// take извлекает накопленное и останавливает таймеры (вызов под мьютексом).
func (s *bufferState) take() []int64 {
	if s.idle != nil {
		s.idle.Stop()
	}
	if s.max != nil {
		s.max.Stop()
	}
	return s.eventIDs
}

// NewChatBufferManager строит менеджер. Pool nil — только с подменённым
// startTurn (юнит-тесты логики окон без БД).
func NewChatBufferManager(deps BufferDeps) *ChatBufferManager {
	logger := deps.Logger
	if logger == nil {
		logger = log.New(discardWriter{}, "", 0)
	}
	m := &ChatBufferManager{
		deps:         deps,
		logger:       logger,
		Windows:      DefaultWindows,
		MaxEvents:    MaxEventsPerBatch,
		MaxTextChars: MaxBatchTextChars,
		buffers:      map[int64]*bufferState{},
	}
	if deps.Pool != nil {
		m.startTurn = m.dbStartTurn
	}
	return m
}

// discardWriter — io.Writer в никуда (log.New(nil,…) паникует на записи).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// Add кладёт pending-событие в буфер чата; таймеры пересчитываются.
func (m *ChatBufferManager) Add(event PendingEvent) {
	m.mu.Lock()
	state, ok := m.buffers[event.ChatID]
	if !ok {
		state = &bufferState{
			chatID:  event.ChatID,
			windows: m.Windows(event.ChatType, event.Kind),
		}
		m.buffers[event.ChatID] = state
		// Max от первого события: поток не переносит закрытие бесконечно (§6.2).
		state.max = time.AfterFunc(state.windows.Max, func() { m.closeChat(event.ChatID) })
	}
	state.eventIDs = append(state.eventIDs, event.EventID)
	if event.Text != nil {
		state.textChars += utf8.RuneCountInString(*event.Text)
	}

	if len(state.eventIDs) >= m.MaxEvents || state.textChars >= m.MaxTextChars {
		ids := state.take()
		delete(m.buffers, event.ChatID)
		m.mu.Unlock()
		m.closeAsync(context.Background(), event.ChatID, ids)
		return
	}
	if state.idle != nil {
		state.idle.Stop()
	}
	state.idle = time.AfterFunc(state.windows.Idle, func() { m.closeChat(event.ChatID) })
	m.mu.Unlock()
}

// closeChat закрывает буфер чата по любому из таймеров; сработавший вторым
// таймер находит пустоту и ничего не делает.
func (m *ChatBufferManager) closeChat(chatID int64) {
	m.mu.Lock()
	state, ok := m.buffers[chatID]
	if !ok {
		m.mu.Unlock()
		return
	}
	ids := state.take()
	delete(m.buffers, chatID)
	m.mu.Unlock()
	m.closeAsync(context.Background(), chatID, ids)
}

// closeAsync выполняет закрытие порции вне ingest-пути: сбой БД оставляет
// события pending — их подберёт RecoverStalePending (§12.2; in-process retry —
// бэклог чек-листа).
func (m *ChatBufferManager) closeAsync(ctx context.Context, chatID int64, ids []int64) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Транзиентный сбой закрытия (таймаут acquire, рестарт PG) повторяем
		// здесь же: периодический recovery — последний пояс, а не первый;
		// без повтора события ждали его и могли обогнать более новый ход.
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			if err = m.createTurn(ctx, chatID, ids); err == nil {
				return
			}
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
			}
		}
		m.logger.Printf("буфер: порция чата %d не закрыта после повторов (%d событий остаются pending до recovery): %v",
			chatID, len(ids), err)
	}()
}

// FlushAll немедленно закрывает все буферы (graceful shutdown) и ждёт
// завершения незакрытых createTurn.
func (m *ChatBufferManager) FlushAll(ctx context.Context) {
	m.mu.Lock()
	chats := make([]int64, 0, len(m.buffers))
	for chatID, state := range m.buffers {
		ids := state.take()
		delete(m.buffers, chatID)
		chats = append(chats, chatID)
		m.wg.Add(1)
		go func(chatID int64, ids []int64) {
			defer m.wg.Done()
			if err := m.createTurn(ctx, chatID, ids); err != nil {
				m.logger.Printf("буфер: порция чата %d не закрыта при остановке: %v", chatID, err)
			}
		}(chatID, ids)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Wait ждёт завершения фоновых закрытий (тесты, shutdown).
func (m *ChatBufferManager) Wait() { m.wg.Wait() }

// RecoverStalePending — recovery (§12.2 «событие pending, turn_started нет —
// восстановить буфер»): таймеры потеряны при падении, pending-события
// закрываются в turn немедленно при старте.
func (m *ChatBufferManager) RecoverStalePending(ctx context.Context) (chats, events int, err error) {
	return m.RecoverStalePendingOlderThan(ctx, 0)
}

// RecoverStalePendingOlderThan — тот же recovery только для событий старше
// age. Периодический вызов в живом процессе (M5) обязан не трогать свежие
// pending: они ещё лежат в буфере и закроются своим таймером. Ошибка одного
// чата логируется и не валит остальные (старт не должен падать из-за гонки
// RowsAffected на одном чате).
func (m *ChatBufferManager) RecoverStalePendingOlderThan(ctx context.Context, age time.Duration) (chats, events int, err error) {
	// События, ещё лежащие в буфере памяти, не трогаем: у них жив свой таймер,
	// а occurred_at — время сообщения Telegram, которое после простоя поллера
	// может быть старым (minor ревью). Пустой срез — не NULL для ANY().
	m.mu.Lock()
	buffered := make([]int64, 0)
	for _, state := range m.buffers {
		buffered = append(buffered, state.eventIDs...)
	}
	m.mu.Unlock()

	rows, err := m.deps.Pool.Query(ctx, `
		SELECT chat_id, array_agg(id) AS ids
		FROM social_events
		WHERE status = 'pending'
		  AND ($1::interval <= interval '0' OR occurred_at < now() - $1::interval)
		  AND NOT (id = ANY($2::bigint[]))
		GROUP BY chat_id`, age, buffered)
	if err != nil {
		return 0, 0, fmt.Errorf("буфер: pending-события не читаются: %w", err)
	}
	defer rows.Close()
	type chatEvents struct {
		chatID int64
		ids    []int64
	}
	var batches []chatEvents
	for rows.Next() {
		var b chatEvents
		if err := rows.Scan(&b.chatID, &b.ids); err != nil {
			return 0, 0, fmt.Errorf("буфер: строка recovery не читается: %w", err)
		}
		batches = append(batches, b)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("буфер: recovery не дочитан: %w", err)
	}
	for _, b := range batches {
		if err := m.createTurn(ctx, b.chatID, b.ids); err != nil {
			m.logger.Printf("буфер: recovery чата %d не удался, остальные чаты продолжаются: %v", b.chatID, err)
			continue
		}
		chats++
		events += len(b.ids)
	}
	return chats, events, nil
}

// createTurn — транзакционное закрытие порции (§6.2): turn с постоянным ID,
// привязка событий — атомарно. Успех нельзя разделить: либо события в turn,
// либо остаются pending.
func (m *ChatBufferManager) createTurn(ctx context.Context, chatID int64, ids []int64) error {
	m.turnMu.Lock()
	turnID, kind, err := m.startTurn(ctx, chatID, ids)
	m.turnMu.Unlock()
	if err != nil {
		return err
	}
	m.logger.Printf("буфер: порция чата %d закрыта в turn %d (kind=%s, событий %d)",
		chatID, turnID, kind, len(ids))
	if m.deps.OnTurnReady != nil {
		m.deps.OnTurnReady(turnID, chatID, kind)
	}
	return nil
}

// dbStartTurn — реализация StartTurnFunc поверх БД: тип чата читается из
// первого события, turn и привязка — одна транзакция.
func (m *ChatBufferManager) dbStartTurn(ctx context.Context, chatID int64, ids []int64) (int64, string, error) {
	if len(ids) == 0 {
		return 0, "", fmt.Errorf("буфер: пустая порция чата %d", chatID)
	}
	var chatType string
	if err := m.deps.Pool.QueryRow(ctx,
		`SELECT chat_type FROM social_events WHERE id = $1`, ids[0]).Scan(&chatType); err != nil {
		return 0, "", fmt.Errorf("буфер: тип чата %d не читается: %w", chatID, err)
	}
	kind := "group"
	if chatType == string(ChatPrivate) {
		kind = "direct"
	}

	tx, err := m.deps.Pool.Begin(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("буфер: транзакция не открыта: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var turnID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO turns (chat_id) VALUES ($1) RETURNING id`, chatID).Scan(&turnID); err != nil {
		return 0, "", fmt.Errorf("буфер: turn не создан: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE social_events SET turn_id = $1, status = 'buffered'
		 WHERE id = ANY($2::bigint[]) AND status = 'pending'`, turnID, ids)
	if err != nil {
		return 0, "", fmt.Errorf("буфер: события не привязаны: %w", err)
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return 0, "", fmt.Errorf("буфер: привязано %d событий из %d (часть уже не pending)",
			tag.RowsAffected(), len(ids))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", fmt.Errorf("буфер: транзакция не зафиксирована: %w", err)
	}
	return turnID, kind, nil
}
