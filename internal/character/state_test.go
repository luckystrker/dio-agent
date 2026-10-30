// Юнит-тесты: отношения §7.3 (описание, лимиты дельт), окно недавнего разговора
// 8.1 (fitHistory), typing-пульс §13.3, детерминированный attemptId бюджета.
package character

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDescribeRelationshipBands(t *testing.T) {
	cases := []struct {
		rel  Relationship
		want string
	}{
		{Relationship{Familiarity: 0.1, Affinity: 0, Trust: 0.2}, "почти незнаком"},
		{Relationship{Familiarity: 0.4, Affinity: 0.5, Trust: 0.7}, "уже неплохо знаком, он тебе симпатичен, доверяешь ему"},
		{Relationship{Familiarity: 0.9, Affinity: -0.5, Trust: 0.1}, "хорошо знаешь, он тебе неприятен, доверия мало"},
	}
	for _, tc := range cases {
		if got := DescribeRelationship(tc.rel); got != tc.want {
			t.Fatalf("DescribeRelationship(%+v) = %q, хочу %q", tc.rel, got, tc.want)
		}
	}
}

func TestRelationshipLimitConstants(t *testing.T) {
	// §7.3: изменения ограничены на ход; familiarity растёт медленнее affinity/trust.
	if RelationshipLimits.AffinityPerTurn != 0.1 || RelationshipLimits.TrustPerTurn != 0.1 {
		t.Fatalf("лимиты affinity/trust расходятся со справочником: %+v", RelationshipLimits)
	}
	if RelationshipLimits.FamiliarityPerSignificantTurn != 0.05 {
		t.Fatalf("лимит familiarity расходится со справочником: %+v", RelationshipLimits)
	}
}

func TestFitHistoryDropsOldestFirst(t *testing.T) {
	messages := []HistoryMessage{
		{Role: "user", Content: "старое сообщение " + strings.Repeat("х", 100)},
		{Role: "assistant", Content: "среднее " + strings.Repeat("у", 50)}, // 58 символов
		{Role: "user", Content: "свежее"},                                  // 6 символов
	}
	// Бюджет 70: свежее (6) + среднее (58) = 64 влезает, старое (115) — нет.
	out := FitHistory(messages, 70)
	if len(out) != 2 {
		t.Fatalf("осталось %d сообщений, хочу 2", len(out))
	}
	if out[0].Content != "среднее "+strings.Repeat("у", 50) {
		t.Fatalf("самое старое не выброшено первым: %v", out)
	}
	if out[1].Content != "свежее" {
		t.Fatalf("свежее потеряно: %v", out)
	}
}

func TestFitHistoryKeepsNewestAlways(t *testing.T) {
	// Бюджет меньше одного сообщения: самое свежее сохраняется всегда (паритет TS).
	out := FitHistory([]HistoryMessage{{Role: "user", Content: strings.Repeat("ы", 200)}}, 10)
	if len(out) != 1 {
		t.Fatalf("свежее сообщение потеряно при малом бюджете")
	}
}

func TestFitHistoryEmptyBudgetKeepsNewest(t *testing.T) {
	out := FitHistory([]HistoryMessage{
		{Role: "user", Content: "а"},
		{Role: "assistant", Content: "б"},
	}, 0)
	if len(out) != 1 || out[0].Content != "б" {
		t.Fatalf("бюджет 0 должен оставить только свежее: %+v", out)
	}
}

func TestAttemptOfSuffix(t *testing.T) {
	// Префикс операции — "model:turn:<id>:<attempt>": попытка всегда последний сегмент.
	if got := attemptOf("model:turn:42:3"); got != 3 {
		t.Fatalf("attemptOf = %d, хочу 3", got)
	}
	if got := attemptOf("model:turn:7:0"); got != 0 {
		t.Fatalf("attemptOf попытки 0 = %d, хочу 0", got)
	}
	if got := attemptOf("model:turn:42:x"); got != 0 {
		t.Fatalf("attemptOf нечислового сегмента = %d, хочу 0", got)
	}
}

// fakeActionSender фиксирует отправленные chat action (потокобезопасно:
// пульс тикает из своей горутины).
type fakeActionSender struct {
	mu      sync.Mutex
	actions []string
}

func (f *fakeActionSender) SendChatAction(_ context.Context, _ int64, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
	return nil
}

func (f *fakeActionSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actions)
}

func TestTypingPulseStartStop(t *testing.T) {
	sender := &fakeActionSender{}
	pulse := NewTypingPulse(sender, 20*time.Millisecond)
	pulse.Start(42)
	time.Sleep(70 * time.Millisecond) // старт + пара тиков
	pulse.Stop(42)
	time.Sleep(30 * time.Millisecond)
	if n := sender.count(); n < 2 {
		t.Fatalf("пульс не обновлялся: %d действий", n)
	}
	// После stop новых действий нет.
	n := sender.count()
	time.Sleep(50 * time.Millisecond)
	if sender.count() != n {
		t.Fatalf("пульс продолжил тикать после stop: %d → %d", n, sender.count())
	}
}

func TestTypingPulseStartReplaces(t *testing.T) {
	// Повторный старт заменяет прежний пульс: старый тикер остановлен (паритет TS).
	sender := &fakeActionSender{}
	pulse := NewTypingPulse(sender, 15*time.Millisecond)
	pulse.Start(1)
	pulse.Start(1)
	time.Sleep(60 * time.Millisecond)
	pulse.Stop(1)
	pulse.Stop(1) // повторный stop безопасен
}

// failingSender отвечает ошибкой — пульс не имеет права ронять ход.
type failingSender struct{}

func (failingSender) SendChatAction(context.Context, int64, string) error {
	return errors.New("telegram недоступен")
}

func TestTypingPulseSwallowsErrors(t *testing.T) {
	pulse := NewTypingPulse(failingSender{}, 10*time.Millisecond)
	pulse.Start(7)
	time.Sleep(40 * time.Millisecond)
	pulse.Stop(7) // дожили без паники
}
