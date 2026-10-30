package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"ago/events"
)

// События WAL должны проходить полный цикл сериализации через реестр ago/events:
// так WAL-записи перечитываются из журнала как полноценные события.
func TestTaskStepEventsRoundTripRegistry(t *testing.T) {
	intended := &TaskStepIntended{
		Step:   "fetch-source",
		Kind:   "explore_fetch",
		Params: map[string]any{"url": "https://example.com", "limit": float64(3)},
	}
	intended.CreatedAt = time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)

	raw, err := events.MarshalEvent(intended)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if intended.EventType() != EventTypeTaskStepIntended {
		t.Errorf("EventType намерения = %q, хочу %q", intended.EventType(), EventTypeTaskStepIntended)
	}

	decoded, err := events.UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got, ok := decoded.(*TaskStepIntended)
	if !ok {
		t.Fatalf("UnmarshalEvent вернул %T, хочу *TaskStepIntended", decoded)
	}
	if got.Step != intended.Step || got.Kind != intended.Kind || !reflect.DeepEqual(got.Params, intended.Params) {
		t.Errorf("событие перечиталось с расхождением: %+v, хочу %+v", got, intended)
	}
	if !got.At().Equal(intended.At()) {
		t.Errorf("время события не сохранилось: %v, хочу %v", got.At(), intended.At())
	}

	done := &TaskStepDone{Step: "fetch-source", Result: map[string]any{"ok": true}}
	done.CreatedAt = intended.CreatedAt
	rawDone, err := events.MarshalEvent(done)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	if done.EventType() != EventTypeTaskStepDone {
		t.Errorf("EventType отметки = %q, хочу %q", done.EventType(), EventTypeTaskStepDone)
	}
	decodedDone, err := events.UnmarshalEvent(rawDone)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	gotDone, ok := decodedDone.(*TaskStepDone)
	if !ok {
		t.Fatalf("UnmarshalEvent вернул %T, хочу *TaskStepDone", decodedDone)
	}
	if gotDone.Step != done.Step || !reflect.DeepEqual(gotDone.Result, done.Result) {
		t.Errorf("отметка перечиталась с расхождением: %+v, хочу %+v", gotDone, done)
	}
}

// DropHistory на долговременном журнале запрещён без обращений к БД.
func TestDropHistoryForbidden(t *testing.T) {
	storage := NewPgStreamStorage(context.Background(), nil)
	if err := storage.DropHistory("s-any"); !errors.Is(err, ErrDropForbidden) {
		t.Errorf("DropHistory вернул %v, хочу ErrDropForbidden", err)
	}
}

// Обрыв последовательности распознаётся по номерам, а не молча пропускается.
func TestCheckContiguous(t *testing.T) {
	if err := checkContiguous("s-1", []int64{1, 2, 3}); err != nil {
		t.Errorf("непрерывная последовательность не должна давать ошибку: %v", err)
	}
	err := checkContiguous("s-1", []int64{1, 2, 4})
	if !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("обрыв 1,2,4 вернул %v, хочу ErrSequenceGap", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("сообщение об ошибке должно называть пропущенный seq 3: %v", err)
	}
	err = checkContiguous("s-1", []int64{2})
	if !errors.Is(err, ErrSequenceGap) {
		t.Errorf("последовательность не с 1 вернула %v, хочу ErrSequenceGap", err)
	}
	if err := checkContiguous("s-1", nil); err != nil {
		t.Errorf("пустой поток не должен давать ошибку: %v", err)
	}
}

// Пустой stream_id отклоняется до обращения к базе.
func TestGetHistoryEmptyStreamID(t *testing.T) {
	storage := NewPgStreamStorage(context.Background(), nil)
	if _, err := storage.GetHistory(""); err == nil {
		t.Fatal("GetHistory с пустым stream_id должен давать ошибку")
	}
}
