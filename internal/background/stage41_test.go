// Тесты этапа 41 в фоне: перенос инициативы, которую не пустил гейт (M6), и
// потолок «прочих» инструментов разговорного хода (m2).
package background

import (
	"testing"
	"time"
)

// TestInitiativeDeferralUntil — M6: пауза между инициативами переносит
// намерение на её окончание, дневной лимит — на следующий дневной слот;
// прочие причины не переносятся (отмена).
func TestInitiativeDeferralUntil(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	loc := time.UTC

	until, ok := initiativeDeferralUntil("достигнут дневной лимит инициатив (2/сутки, §14.3)", KindCheckIn, now, loc)
	if !ok || !until.Equal(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("дневной лимит → %v/%v, хочу завтра 10:00", until, ok)
	}

	until, ok = initiativeDeferralUntil("между инициативами прошло меньше 4h0m0s (§14.3)", KindReplyLater, now, loc)
	if !ok || !until.Equal(now.Add(MinProactiveGap)) {
		t.Fatalf("пауза → %v/%v, хочу now+%v", until, ok, MinProactiveGap)
	}

	if _, ok := initiativeDeferralUntil("чат заблокировал бота — инициативы не отправляются (§13.2, 38.7)", KindReplyLater, now, loc); ok {
		t.Fatal("блокировка чата не должна переносить намерение")
	}
	// check_in/share с паузой по-прежнему отменяются, а не переносятся.
	if _, ok := initiativeDeferralUntil("между инициативами прошло меньше 4h0m0s (§14.3)", KindCheckIn, now, loc); ok {
		t.Fatal("check_in не должен переноситься по паузе между инициативами")
	}
}

// TestDailyLimitedKindOnlyShareCheckIn — M6: reply_later не под дневным
// лимитом инициатив (иначе defer при исчерпанном лимите теряется).
func TestDailyLimitedKindOnlyShareCheckIn(t *testing.T) {
	if isDailyLimitedKind(KindReplyLater) {
		t.Fatal("reply_later попал под дневной лимит инициатив")
	}
	for _, kind := range []string{KindCheckIn, KindShare} {
		if !isDailyLimitedKind(kind) {
			t.Fatalf("%q не под дневным лимитом", kind)
		}
	}
}
