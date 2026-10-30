//go:build db

// БД-тесты адаптера чекпоинтов (требование 9.2): round-trip состояние ->
// task_checkpoints -> состояние, перезапись прежнего чекпоинта, отказ на
// не-JSON и возобновление долгой задачи средствами ago/task поверх адаптера.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/stream"
	agotask "github.com/luckystrker/ago/task"
)

// Чекпоинт round-trip (требование 9.2): записанное состояние читается тем же
// JSON-значением; jsonb канонизирует формат, поэтому сверяем значение.
func TestPgCheckpointStoreRoundTrip(t *testing.T) {
	store := NewPgCheckpointStore(context.Background(), newTestPool(t))

	// Нет чекпоинта — (nil, nil) по контракту интерфейса ago/task.
	state, err := store.Read("task-absent")
	if err != nil {
		t.Fatalf("Read по отсутствующему чекпоинту: %v", err)
	}
	if state != nil {
		t.Fatalf("Read по отсутствующему чекпоинту вернул %s, хочу nil", state)
	}

	want := map[string]any{
		"step":    float64(3),
		"episode": "ep-42",
		"done":    []any{"fetch", "summarize"},
	}
	rawWant, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("маршалинг состояния: %v", err)
	}
	if err := store.Write("task-1", rawWant); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := store.Read("task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var gotValue map[string]any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("перечитанный чекпоинт не JSON: %v (%s)", err, got)
	}
	if !reflect.DeepEqual(want, gotValue) {
		t.Fatalf("round-trip потерял значение: %v, хочу %v", gotValue, want)
	}

	// Перезапись: прежний чекпоинт заменяется (паритет MemoryCheckpointStore).
	if err := store.Write("task-1", []byte(`{"step":7}`)); err != nil {
		t.Fatalf("перезапись: %v", err)
	}
	got, err = store.Read("task-1")
	if err != nil {
		t.Fatalf("Read после перезаписи: %v", err)
	}
	var rewritten map[string]any
	if err := json.Unmarshal(got, &rewritten); err != nil {
		t.Fatalf("чекпоинт после перезаписи не JSON: %v (%s)", err, got)
	}
	if !reflect.DeepEqual(map[string]any{"step": float64(7)}, rewritten) {
		t.Fatalf("после перезаписи чекпоинт = %v, хочу {\"step\": 7}", rewritten)
	}

	// Не-JSON и пустое состояние отклоняются, ничего не записывается.
	if err := store.Write("task-2", []byte("{не json")); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Write не-JSON вернул %v, хочу ErrInvalidCheckpoint", err)
	}
	if err := store.Write("task-2", nil); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Write пустого состояния вернул %v, хочу ErrInvalidCheckpoint", err)
	}
	state, err = store.Read("task-2")
	if err != nil || state != nil {
		t.Fatalf("после отказанных Write чекпоинт task-2 = (%v, %v), хочу (nil, nil)", state, err)
	}
}

// Возобновление долгой задачи средствами ago/task (§12.2 «Незавершённая
// фоновая задача — продолжить с чекпоинта»): Checkpoint первой задачи читается
// через WithResumeFrom второй.
func TestPgCheckpointStoreResumesAgoTask(t *testing.T) {
	store := NewPgCheckpointStore(context.Background(), newTestPool(t))

	state := []byte(`{"step":2,"next":"summarize"}`)

	// Первый запуск: задача пишёт чекпоинт и завершается.
	firstStream := stream.NewMemoryStream(stream.WithStreamID("wal-checkpoint-run-1"))
	t1, err := agotask.Start(agocontext.New(firstStream), "runtime-test",
		agotask.Spec{Title: "долгая задача"}, agotask.WithCheckpointStore(store))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := t1.Checkpoint(state); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := t1.Complete(nil); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Перезапуск: новая задача продолжается с чекпоинта прежней.
	secondStream := stream.NewMemoryStream(stream.WithStreamID("wal-checkpoint-run-2"))
	t2, err := agotask.Start(agocontext.New(secondStream), "runtime-test",
		agotask.Spec{Title: "долгая задача"},
		agotask.WithCheckpointStore(store), agotask.WithResumeFrom(t1.TaskID()))
	if err != nil {
		t.Fatalf("Start с возобновлением: %v", err)
	}
	resumed := t2.ResumedState()
	if resumed == nil {
		t.Fatal("ResumedState пустой — чекпоинт не поднялся")
	}

	var got map[string]any
	if err := json.Unmarshal(resumed, &got); err != nil {
		t.Fatalf("ResumedState не JSON: %v (%s)", err, resumed)
	}
	var want map[string]any
	if err := json.Unmarshal(state, &want); err != nil {
		t.Fatalf("исходное состояние не JSON: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("возобновлённое состояние %v, хочу %v", got, want)
	}
	t2.Complete(nil)
}
