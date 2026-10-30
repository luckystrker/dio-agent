// WAL фоновых задач (§12.2, §18): «сначала запиши, потом делай» — намерение
// шага долгой задачи попадает в событийный журнал до исполнения шага; после
// сбоя видно, что делалось, и продолжение идёт от чекпоинта и последнего
// завершённого шага. Отдельной таблицы нет: WAL ведётся в event_streams
// отдельными потоками — поток задачи (stream_id = task_id).
//
// Шаги WAL — полноценные события ago (зарегистрированы в реестре events),
// поэтому пишутся через PgStreamStorage с теми же гарантиями однократности
// (UNIQUE(stream_id, seq)) и append-only, а перечитываются декодированными.
package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/luckystrker/ago/events"
)

// Типы событий WAL фоновых задач (колонка event_streams.type).
const (
	// EventTypeTaskStepIntended — намерение шага, записывается ДО исполнения.
	EventTypeTaskStepIntended = "task_step_intended"
	// EventTypeTaskStepDone — шаг исполнен, записывается после успеха.
	EventTypeTaskStepDone = "task_step_done"
)

// TaskStepIntended — намерение шага: что собираемся сделать. Params —
// параметры шага (jsonb-объект, владелец задачи выбирает содержание).
type TaskStepIntended struct {
	events.Base
	Step   string         `json:"step"` // имя шага внутри задачи
	Kind   string         `json:"kind"` // род работы, например "consolidate", "explore_fetch"
	Params map[string]any `json:"params,omitempty"`
}

func (TaskStepIntended) EventType() string    { return EventTypeTaskStepIntended }
func (TaskStepIntended) Transient() bool      { return false }
func (TaskStepIntended) Conversational() bool { return false } // WAL задач — не реплика модели

// TaskStepDone — отметка об исполненном шаге. Result — краткий итог шага
// для наблюдаемости; содержательные результаты живут в своих таблицах.
type TaskStepDone struct {
	events.Base
	Step   string         `json:"step"`
	Result map[string]any `json:"result,omitempty"`
}

func (TaskStepDone) EventType() string    { return EventTypeTaskStepDone }
func (TaskStepDone) Transient() bool      { return false }
func (TaskStepDone) Conversational() bool { return false }

func init() {
	events.RegisterEvent(TaskStepIntended{})
	events.RegisterEvent(TaskStepDone{})
}

// StepRecord — один шаг в перечитанном WAL задачи.
type StepRecord struct {
	Seq    int64
	Step   string
	Done   bool           // false = намерение без отметки об исполнении
	Kind   string         // только для намерений
	Params map[string]any // только для намерений
	Result map[string]any // только для исполненных
	At     time.Time
}

// TaskWAL — фасад WAL фоновых задач над журналом (поток = задача).
type TaskWAL struct {
	journal *PgStreamStorage
}

// NewTaskWAL создаёт WAL поверх адаптера журнала.
func NewTaskWAL(journal *PgStreamStorage) *TaskWAL {
	return &TaskWAL{journal: journal}
}

// PlanStep записывает намерение шага до его исполнения (§12.2: после сбоя
// видно, что делалось).
func (w *TaskWAL) PlanStep(ctx context.Context, taskID, step, kind string, params map[string]any) error {
	ev := &TaskStepIntended{Step: step, Kind: kind, Params: params}
	ev.CreatedAt = time.Now().UTC()
	return w.journal.append(ctx, taskID, ev)
}

// StepDone отмечает исполненный шаг. Вызывается только после фактического
// успеха: «intended без done» в перечитанном WAL — признак оборванного шага.
func (w *TaskWAL) StepDone(ctx context.Context, taskID, step string, result map[string]any) error {
	ev := &TaskStepDone{Step: step, Result: result}
	ev.CreatedAt = time.Now().UTC()
	return w.journal.append(ctx, taskID, ev)
}

// Steps перечитывает WAL задачи для восстановления: шаги по порядку с
// отметками. События других типов в потоке задачи (например события
// жизненного цикла ago/task) пропускаются.
func (w *TaskWAL) Steps(ctx context.Context, taskID string) ([]StepRecord, error) {
	evs, err := w.journal.getHistory(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("WAL: поток задачи %q не читается: %w", taskID, err)
	}
	out := make([]StepRecord, 0, len(evs))
	for seq, ev := range evs {
		switch step := ev.(type) {
		case *TaskStepIntended:
			out = append(out, StepRecord{
				Seq:    int64(seq + 1),
				Step:   step.Step,
				Done:   false,
				Kind:   step.Kind,
				Params: step.Params,
				At:     step.At(),
			})
		case *TaskStepDone:
			out = append(out, StepRecord{
				Seq:    int64(seq + 1),
				Step:   step.Step,
				Done:   true,
				Result: step.Result,
				At:     step.At(),
			})
		}
	}
	return out, nil
}
