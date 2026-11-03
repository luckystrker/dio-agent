// События хода в событийном журнале (§12.1, §18 event_streams). Минимальный
// набор: turn_started, tool_call, tool_result, model_output, deltas_applied,
// delivery_queued, turn_skipped, turn_failed. Единица записи — событие; stream —
// идентификатор хода ("turn:<id>"), UNIQUE(stream_id, seq) даёт однократность
// записи, из которой выводится однократность применения хода (инвариант 4).
//
// События — полноценные события ago (зарегистрированы в реестре events), как
// WAL фоновых задач в internal/runtime/wal.go: пишутся адаптером stream.Storage
// (9.2) с гарантиями однократности и append-only, читаются декодированными для
// восстановления §12.2. Траектория ago-запуска (нативные ModelRequest /
// ToolCallsEvent / ModelResponse) в долговременный журнал не пишется —
// фиксацию хода даёт этот прикладной набор; иначе ход фиксировался бы дважды.
package character

import (
	"encoding/json"

	"github.com/luckystrker/ago/events"
)

// Типы событий хода (колонка event_streams.type, §12.1).
const (
	EventTypeTurnStarted    = "turn_started"
	EventTypeToolCall       = "tool_call"
	EventTypeToolResult     = "tool_result"
	EventTypeModelOutput    = "model_output"
	EventTypeDeltasApplied  = "deltas_applied"
	EventTypeDeliveryQueued = "delivery_queued"
	EventTypeTurnSkipped    = "turn_skipped"
	EventTypeTurnFailed     = "turn_failed"
)

// TurnStarted — начало хода: пачка или цель фоновой задачи, модель/бюджет.
type TurnStarted struct {
	events.Base
	TurnID       int64  `json:"turn_id"`
	ChatID       int64  `json:"chat_id"`
	Kind         string `json:"kind"` // conversation | background
	BatchCount   int    `json:"batch_count"`
	Direct       bool   `json:"direct,omitempty"`
	Model        string `json:"model,omitempty"`
	BudgetOpID   string `json:"budget_operation_id,omitempty"`
	ContextRevis int64  `json:"context_revision"`
}

func (TurnStarted) EventType() string    { return EventTypeTurnStarted }
func (TurnStarted) Transient() bool      { return false }
func (TurnStarted) Conversational() bool { return false } // журнал хода — не реплика модели

// ToolCallRecorded — обращение к инструменту памяти (аргументы модели).
type ToolCallRecorded struct {
	events.Base
	CallID    string `json:"call_id"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
}

func (ToolCallRecorded) EventType() string    { return EventTypeToolCall }
func (ToolCallRecorded) Transient() bool      { return false }
func (ToolCallRecorded) Conversational() bool { return false }

// ToolResultRecorded — результат обращения к инструменту (или его ошибка).
type ToolResultRecorded struct {
	events.Base
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	Error  string `json:"error,omitempty"` // непусто — вызов завершился ошибкой
	Result string `json:"result,omitempty"`
}

func (ToolResultRecorded) EventType() string    { return EventTypeToolResult }
func (ToolResultRecorded) Transient() bool      { return false }
func (ToolResultRecorded) Conversational() bool { return false }

// ModelOutput — валидированный TurnResult (§11.2). Пишется ДО применения дельт:
// сбой между событиями восстанавливает ход применением сохранённого результата
// без новой генерации (§12.1, §12.2). Usage — фактические токены вызова (§20).
// Reasoning — рассуждение модели, если провайдер его отдал (26.3): только
// журнал, в контекст/историю не возвращается; пусто — поля в JSON нет.
type ModelOutput struct {
	events.Base
	Result          json.RawMessage `json:"result"`
	Usage           map[string]any  `json:"usage,omitempty"`
	Repairs         int             `json:"repairs,omitempty"` // сколько repair-попыток потребовалось
	Reasoning       string          `json:"reasoning,omitempty"`
	ReasoningRepair string          `json:"reasoning_repair,omitempty"`
}

func (ModelOutput) EventType() string    { return EventTypeModelOutput }
func (ModelOutput) Transient() bool      { return false }
func (ModelOutput) Conversational() bool { return false }

// DeltasApplied — применённые изменения mood/relationships/memory/intentions
// одним событием-списком (§12.1). Основания (reason) изменений живут здесь —
// журнал и есть аудит-след вместо таблицы state_notes TS-версии.
type DeltasApplied struct {
	events.Base
	Mood          *MoodDelta                 `json:"mood,omitempty"`
	Relationships []AppliedRelationshipDelta `json:"relationships,omitempty"`
	MemoryIDs     []int64                    `json:"memory_ids,omitempty"`
	IntentionIDs  []int64                    `json:"intention_ids,omitempty"`
	Familiarity   map[string]float64         `json:"familiarity,omitempty"` // "entityID:scope" → новое значение
	Reason        string                     `json:"reason,omitempty"`
}

func (DeltasApplied) EventType() string    { return EventTypeDeltasApplied }
func (DeltasApplied) Transient() bool      { return false }
func (DeltasApplied) Conversational() bool { return false }

// AppliedRelationshipDelta — применённое (сжатое) изменение отношений.
type AppliedRelationshipDelta struct {
	PersonEntityID int64    `json:"person_entity_id"`
	ScopeID        string   `json:"scope_id"`
	Affinity       *float64 `json:"affinity,omitempty"`
	Trust          *float64 `json:"trust,omitempty"`
	Reason         string   `json:"reason"`
}

// DeliveryQueued — созданные outbox rows (§13). Событие и строки пишутся
// в одной транзакции с дельтами.
type DeliveryQueued struct {
	events.Base
	TurnID int64            `json:"turn_id"`
	ChatID int64            `json:"chat_id"`
	Outbox []QueuedDelivery `json:"outbox"`
}

func (DeliveryQueued) EventType() string    { return EventTypeDeliveryQueued }
func (DeliveryQueued) Transient() bool      { return false }
func (DeliveryQueued) Conversational() bool { return false }

// QueuedDelivery — одна строка outbox в событии.
type QueuedDelivery struct {
	ID       int64  `json:"id"`
	Position int    `json:"position"`
	Kind     string `json:"kind"` // message | reaction | sticker
	Text     string `json:"text,omitempty"`
	Reaction string `json:"reaction,omitempty"`
	FileID   string `json:"file_id,omitempty"` // стикер: file_id из коллекции
}

// TurnSkipped — терминальный пропуск с причиной (инвариант 5): после события
// ход больше не генерируется и не применяется.
type TurnSkipped struct {
	events.Base
	TurnID int64  `json:"turn_id"`
	Phase  string `json:"phase"` // generate | apply | worker | recovery
	Reason string `json:"reason"`
}

func (TurnSkipped) EventType() string    { return EventTypeTurnSkipped }
func (TurnSkipped) Transient() bool      { return false }
func (TurnSkipped) Conversational() bool { return false }

// TurnFailed — терминальный провал хода с причиной (29.1, §11.3): попытки
// исчерпаны, а «отравленный» результат/сбой применения не даёт ходу
// завершиться. Отличается от TermSkipped (сознательный пропуск): провал —
// ошибка исполнения, а не решение не обрабатывать событие. После этого
// события ход не генерируется и не применяется; воркер строку failed не
// берёт, recovery её не воскрешает.
type TurnFailed struct {
	events.Base
	TurnID int64  `json:"turn_id"`
	Phase  string `json:"phase"` // worker | apply | recovery
	Reason string `json:"reason"`
}

func (TurnFailed) EventType() string    { return EventTypeTurnFailed }
func (TurnFailed) Transient() bool      { return false }
func (TurnFailed) Conversational() bool { return false }

// init регистрирует события в реестре ago — декодирование при чтении журнала.
func init() {
	events.RegisterEvent(TurnStarted{})
	events.RegisterEvent(ToolCallRecorded{})
	events.RegisterEvent(ToolResultRecorded{})
	events.RegisterEvent(ModelOutput{})
	events.RegisterEvent(DeltasApplied{})
	events.RegisterEvent(DeliveryQueued{})
	events.RegisterEvent(TurnSkipped{})
	events.RegisterEvent(TurnFailed{})
}
