package telegram

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestApplyPrivateAllowlist(t *testing.T) {
	allow := NewPrivateAllowlist([]int64{722958928})
	cases := []struct {
		name  string
		allow PrivateAllowlist
		event NormalizedEvent
		in    IngestStatus
		want  IngestStatus
	}{
		{"личка из списка — в ход", allow,
			NormalizedEvent{ChatType: ChatPrivate, ChatID: 722958928}, StatusPending, StatusPending},
		{"личка чужого — только журнал", allow,
			NormalizedEvent{ChatType: ChatPrivate, ChatID: 42}, StatusPending, StatusJournalOnly},
		{"группа не фильтруется", allow,
			NormalizedEvent{ChatType: ChatGroup, ChatID: -100}, StatusPending, StatusPending},
		{"пустой список — личка открыта всем", nil,
			NormalizedEvent{ChatType: ChatPrivate, ChatID: 42}, StatusPending, StatusPending},
		{"journal_only не повышается", allow,
			NormalizedEvent{ChatType: ChatPrivate, ChatID: 722958928}, StatusJournalOnly, StatusJournalOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.allow.Apply(tc.event, tc.in); got != tc.want {
				t.Fatalf("статус = %s, хочу %s", got, tc.want)
			}
		})
	}
}

// recordingAPI — фейк Bot API: считает исходящие вызовы.
type recordingAPI struct {
	API
	calls []string
}

func (r *recordingAPI) SendMessage(_ context.Context, chatID int64, _ string, _ int64) (int64, error) {
	r.calls = append(r.calls, fmt.Sprintf("sendMessage:%d", chatID))
	return 1, nil
}
func (r *recordingAPI) SendSticker(_ context.Context, chatID int64, _ string) (int64, error) {
	r.calls = append(r.calls, fmt.Sprintf("sendSticker:%d", chatID))
	return 1, nil
}
func (r *recordingAPI) SendPhoto(_ context.Context, chatID int64, _, _ string, _ []byte) (int64, error) {
	r.calls = append(r.calls, fmt.Sprintf("sendPhoto:%d", chatID))
	return 1, nil
}
func (r *recordingAPI) ForwardMessage(_ context.Context, chatID, _, _ int64) (int64, error) {
	r.calls = append(r.calls, fmt.Sprintf("forwardMessage:%d", chatID))
	return 1, nil
}
func (r *recordingAPI) SendChatAction(_ context.Context, chatID int64, _ string) error {
	r.calls = append(r.calls, fmt.Sprintf("sendChatAction:%d", chatID))
	return nil
}
func (r *recordingAPI) SetMessageReaction(_ context.Context, chatID, _ int64, _ string) error {
	r.calls = append(r.calls, fmt.Sprintf("setMessageReaction:%d", chatID))
	return nil
}

func TestGuardAPIBlocksForeignPrivateChats(t *testing.T) {
	ctx := context.Background()
	inner := &recordingAPI{}
	api := GuardAPI(inner, NewPrivateAllowlist([]int64{722958928}))

	// Личка из списка и группа (отрицательный id) — проходят.
	for _, chatID := range []int64{722958928, -100500} {
		if _, err := api.SendMessage(ctx, chatID, "x", 0); err != nil {
			t.Fatalf("чат %d не должен блокироваться: %v", chatID, err)
		}
	}

	// Чужая личка — ни один исходящий метод не доходит до Telegram,
	// ошибка — постоянный 403, который delivery помечает блокировкой.
	const stranger = int64(42)
	checks := []func() error{
		func() error { _, err := api.SendMessage(ctx, stranger, "x", 0); return err },
		func() error { _, err := api.SendSticker(ctx, stranger, "f"); return err },
		func() error { _, err := api.SendPhoto(ctx, stranger, "a.png", "image/png", nil); return err },
		func() error { _, err := api.ForwardMessage(ctx, stranger, -1, 1); return err },
		func() error { return api.SendChatAction(ctx, stranger, "typing") },
		func() error { return api.SetMessageReaction(ctx, stranger, 1, "👍") },
	}
	for i, call := range checks {
		err := call()
		if err == nil || !ChatBlockedError(err) {
			t.Fatalf("вызов %d в чужую личку: хочу ChatBlockedError, получено %v", i, err)
		}
		if outcome := classifySendError(err, 1, 5, time.Second); outcome.kind != outcomeFailed {
			t.Fatalf("вызов %d: отказ whitelist должен быть failed без повторов, получено %v", i, outcome.kind)
		}
	}
	if want := []string{"sendMessage:722958928", "sendMessage:-100500"}; !reflect.DeepEqual(inner.calls, want) {
		t.Fatalf("в Telegram ушло %v, хочу %v", inner.calls, want)
	}

	// Пустой список — обёртки нет.
	if got := GuardAPI(inner, nil); got != API(inner) {
		t.Fatalf("без whitelist API должен остаться прежним")
	}
}
