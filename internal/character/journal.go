// Фасад журнала хода (§12.1) поверх адаптера stream.Storage (9.2): запись
// событий хода с гарантиями однократности и чтение для восстановления (§12.2).
package character

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"ago/events"

	"github.com/luckystrker/dio-agent/internal/runtime"
)

// TurnStreamID — идентификатор потока хода в журнале.
func TurnStreamID(turnID int64) string { return fmt.Sprintf("turn:%d", turnID) }

// TurnJournal — запись и чтение событий одного хода. Все записи одного потока
// сериализуются мьютексом: параллельные вызовы инструментов одного хода не
// конфликтуют на префиксе (UNIQUE(stream_id, seq) остаётся поясом против
// конкуренции процессов).
type TurnJournal struct {
	storage *runtime.PgStreamStorage
	logger  *log.Logger
	mu      sync.Mutex
}

// NewTurnJournal строит фасад поверх адаптера журнала.
func NewTurnJournal(storage *runtime.PgStreamStorage, logger *log.Logger) *TurnJournal {
	if logger == nil {
		logger = discardLogger()
	}
	return &TurnJournal{storage: storage, logger: logger}
}

// History читает события потока хода (для восстановления §12.2).
func (j *TurnJournal) History(ctx context.Context, turnID int64) ([]events.Event, error) {
	evs, err := j.storage.GetHistory(TurnStreamID(turnID))
	if err != nil {
		return nil, fmt.Errorf("журнал хода: %w", err)
	}
	return evs, nil
}

// appendOnce дописывает событие, если события того же типа в потоке ещё нет
// (turn_started повторного прогона не дублируется); SetHistory сверяет префикс
// и вставляет только суффикс — однократность записи §12.1.
func (j *TurnJournal) appendOnce(ctx context.Context, turnID int64, ev events.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	streamID := TurnStreamID(turnID)
	stored, err := j.storage.GetHistory(streamID)
	if err != nil {
		return err
	}
	kind := ev.EventType()
	for _, s := range stored {
		if s.EventType() == kind {
			return nil // уже записано: повторный прогон не дублирует
		}
	}
	evStamped := stamped(ev)
	return j.storage.SetHistory(streamID, append(stored, evStamped))
}

// append дописывает событие в конец потока (последовательные события хода:
// model_output и др.).
func (j *TurnJournal) append(ctx context.Context, turnID int64, ev events.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	streamID := TurnStreamID(turnID)
	stored, err := j.storage.GetHistory(streamID)
	if err != nil {
		return err
	}
	return j.storage.SetHistory(streamID, append(stored, stamped(ev)))
}

// stamped фиксирует время события: повторный прогон того же события предлагал
// бы тот же payload, но события всегда пишутся ровно один раз (appendOnce/tx).
func stamped(ev events.Event) events.Event {
	switch e := ev.(type) {
	case *TurnStarted:
		stampBase(&e.Base)
	case *ToolCallRecorded:
		stampBase(&e.Base)
	case *ToolResultRecorded:
		stampBase(&e.Base)
	case *ModelOutput:
		stampBase(&e.Base)
	case *DeltasApplied:
		stampBase(&e.Base)
	case *DeliveryQueued:
		stampBase(&e.Base)
	case *TurnSkipped:
		stampBase(&e.Base)
	}
	return ev
}

func stampBase(b *events.Base) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
}

// WriteTurnStarted записывает начало хода (однократно).
func (j *TurnJournal) WriteTurnStarted(ctx context.Context, ev *TurnStarted) error {
	return j.appendOnce(ctx, ev.TurnID, ev)
}

// WriteToolCallFor — фиксация вызова инструмента конкретного хода (§12.1).
func (j *TurnJournal) WriteToolCallFor(ctx context.Context, turnID int64, ev *ToolCallRecorded) error {
	return j.append(ctx, turnID, ev)
}

// WriteToolResultFor — фиксация результата инструмента конкретного хода (§12.1).
func (j *TurnJournal) WriteToolResultFor(ctx context.Context, turnID int64, ev *ToolResultRecorded) error {
	return j.append(ctx, turnID, ev)
}

// WriteModelOutput записывает валидированный результат ДО применения дельт (§12.1).
func (j *TurnJournal) WriteModelOutput(ctx context.Context, turnID int64, result TurnResult, usage map[string]any, repairs int) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("журнал хода: результат не сериализуется: %w", err)
	}
	return j.append(ctx, turnID, &ModelOutput{Result: raw, Usage: usage, Repairs: repairs})
}

// WriteTurnSkipped записывает терминальный пропуск (инвариант 5).
func (j *TurnJournal) WriteTurnSkipped(ctx context.Context, turnID int64, phase, reason string) error {
	return j.appendOnce(ctx, turnID, &TurnSkipped{TurnID: turnID, Phase: phase, Reason: reason})
}

// WriteApplyTx пишет deltas_applied + delivery_queued ВНУТРИ транзакции
// применения (§12.1: дельты + outbox + события — всё или ничего).
func (j *TurnJournal) WriteApplyTx(ctx context.Context, tx pgx.Tx, turnID int64, deltas *DeltasApplied, delivery *DeliveryQueued) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	streamID := TurnStreamID(turnID)
	stored, err := j.storage.GetHistory(streamID)
	if err != nil {
		return err
	}
	proposed := make([]events.Event, len(stored), len(stored)+2)
	copy(proposed, stored)
	proposed = append(proposed, stamped(deltas))
	if delivery != nil {
		proposed = append(proposed, stamped(delivery))
	}
	return j.storage.SetHistoryTx(ctx, tx, streamID, proposed)
}

// TurnState — восстановительное состояние хода по журналу (§12.2).
type TurnState struct {
	Started    bool
	Output     *TurnResult // валидированный результат из model_output, если есть
	Usage      map[string]any
	Repairs    int
	Applied    bool // deltas_applied записан — ход применён, повтор невозможен
	Delivered  bool // delivery_queued записан
	SkipReason string
	Skipped    bool // turn_skipped — терминальный пропуск
}

// LoadTurnState читает состояние хода из журнала (§12.2: recovery есть
// перечитывание, не ручные флаги).
func LoadTurnState(ctx context.Context, j *TurnJournal, turnID int64) (TurnState, error) {
	evs, err := j.History(ctx, turnID)
	if err != nil {
		return TurnState{}, err
	}
	return stateOfEvents(evs), nil
}

func stateOfEvents(evs []events.Event) TurnState {
	var st TurnState
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case TurnStarted:
			st.Started = true
		case ModelOutput:
			var r TurnResult
			if err := json.Unmarshal(e.Result, &r); err == nil {
				st.Output = &r
			}
			st.Usage = e.Usage
			st.Repairs = e.Repairs
		case DeltasApplied:
			st.Applied = true
		case DeliveryQueued:
			st.Delivered = true
		case TurnSkipped:
			st.Skipped = true
			st.SkipReason = e.Reason
		}
	}
	return st
}
