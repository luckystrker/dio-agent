//go:build db

// БД-тесты WAL фоновых задач (требование 9.2, §12.2): намерение шага пишется
// в event_streams до исполнения (поток = задача), отметка об исполнении —
// после; перечитанный WAL показывает оборванный шаг как «intended без done».
package runtime

import (
	"context"
	"reflect"
	"testing"
)

// Полный цикл: план -> done -> план; WAL перечитывается новым экземпляром
// («перезапуск») с сохранением порядка и содержимого.
func TestTaskWALPlanDoneSteps(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	taskID := "background-task-1"
	wal := NewTaskWAL(NewPgStreamStorage(ctx, pool))

	err := wal.PlanStep(ctx, taskID, "fetch-source", "explore_fetch",
		map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("PlanStep: %v", err)
	}
	err = wal.StepDone(ctx, taskID, "fetch-source", map[string]any{"ok": true})
	if err != nil {
		t.Fatalf("StepDone: %v", err)
	}
	err = wal.PlanStep(ctx, taskID, "summarize", "consolidate", nil)
	if err != nil {
		t.Fatalf("PlanStep второй шаг: %v", err)
	}

	steps, err := wal.Steps(ctx, taskID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("WAL задачи содержит %d записей, хочу 3", len(steps))
	}

	// Порядок и содержание: seq 1..3, поток отдельный — stream_id = task_id.
	if steps[0].Seq != 1 || steps[1].Seq != 2 || steps[2].Seq != 3 {
		t.Fatalf("номера шагов в WAL = %d,%d,%d, хочу 1,2,3", steps[0].Seq, steps[1].Seq, steps[2].Seq)
	}
	if steps[0].Done || steps[0].Step != "fetch-source" || steps[0].Kind != "explore_fetch" ||
		!reflect.DeepEqual(steps[0].Params, map[string]any{"url": "https://example.com"}) {
		t.Errorf("запись 1 не совпала с намерением: %+v", steps[0])
	}
	if !steps[1].Done || steps[1].Step != "fetch-source" ||
		!reflect.DeepEqual(steps[1].Result, map[string]any{"ok": true}) {
		t.Errorf("запись 2 не совпала с отметкой об исполнении: %+v", steps[1])
	}
	if steps[2].Done || steps[2].Step != "summarize" || steps[2].Kind != "consolidate" {
		t.Errorf("запись 3 не совпала со вторым намерением: %+v", steps[2])
	}
	if steps[0].At.IsZero() || steps[1].At.IsZero() {
		t.Error("время событий WAL должно проставляться при записи")
	}

	// Колонка type различает намерения и отметки — проверки восстановления §12.2
	// должны работать на уровне SQL.
	storage := NewPgStreamStorage(ctx, pool)
	if n := countByType(t, storage, taskID, EventTypeTaskStepIntended); n != 2 {
		t.Errorf("строк с type=%s: %d, хочу 2", EventTypeTaskStepIntended, n)
	}
	if n := countByType(t, storage, taskID, EventTypeTaskStepDone); n != 1 {
		t.Errorf("строк с type=%s: %d, хочу 1", EventTypeTaskStepDone, n)
	}

	// «Перезапуск»: новый WAL поверх нового адаптера читает тот же WAL задачи.
	afterRestart := NewTaskWAL(NewPgStreamStorage(ctx, pool))
	reread, err := afterRestart.Steps(ctx, taskID)
	if err != nil {
		t.Fatalf("Steps после перезапуска: %v", err)
	}
	if !reflect.DeepEqual(steps, reread) {
		t.Fatalf("WAL после перезапуска не совпал:\n %+v\n %+v", reread, steps)
	}
}

// Обрыв исполнения виден из WAL (§12.2): последний шаг с намерением без
// отметки done — признак оборванного шага, продолжение идёт от чекпоинта.
func TestTaskWALCrashVisibility(t *testing.T) {
	ctx := context.Background()
	wal := NewTaskWAL(NewPgStreamStorage(ctx, newTestPool(t)))

	taskID := "background-task-crashed"
	if err := wal.PlanStep(ctx, taskID, "step-1", "explore_fetch", nil); err != nil {
		t.Fatalf("PlanStep: %v", err)
	}
	if err := wal.StepDone(ctx, taskID, "step-1", nil); err != nil {
		t.Fatalf("StepDone: %v", err)
	}
	// Шаг начат и не завершён: намерение записано, done нет.
	if err := wal.PlanStep(ctx, taskID, "step-2", "consolidate", nil); err != nil {
		t.Fatalf("PlanStep: %v", err)
	}

	steps, err := wal.Steps(ctx, taskID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("WAL задачи содержит %d записей, хочу 3", len(steps))
	}
	last := steps[len(steps)-1]
	if last.Done {
		t.Fatalf("последняя запись WAL помечена исполненной: %+v — оборванный шаг должен выглядеть как намерение без done", last)
	}
	if last.Step != "step-2" {
		t.Fatalf("последняя запись = шаг %q, хочу %q", last.Step, "step-2")
	}
	// Завершённые шаги остаются различимы: recovery продолжит после step-1.
	if !steps[1].Done || steps[1].Step != "step-1" {
		t.Errorf("исполненный шаг не распознан: %+v", steps[1])
	}
}

// countByType считает события потока по колонке type напрямую в базе.
func countByType(t *testing.T, storage *PgStreamStorage, streamID, eventType string) int {
	t.Helper()
	var n int
	if err := storage.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM event_streams WHERE stream_id = $1 AND type = $2`,
		streamID, eventType).Scan(&n); err != nil {
		t.Fatalf("count(*) по type %s: %v", eventType, err)
	}
	return n
}
