// Юнит-тесты этапа 33.2: дневной лимит инициатив чата (§14.3) — один расчёт
// для гейта фона, панели состояния и применения хода.
package character

import "testing"

func TestInitiativeDailyExhausted(t *testing.T) {
	if InitiativeDailyExhausted(InitiativeUsage{Today: InitiativePerChatPerDay - 1}) {
		t.Fatalf("лимит исчерпан раньше счёта %d", InitiativePerChatPerDay)
	}
	if !InitiativeDailyExhausted(InitiativeUsage{Today: InitiativePerChatPerDay}) {
		t.Fatalf("счёт %d не считается исчерпанным лимитом", InitiativePerChatPerDay)
	}
	if !InitiativeDailyExhausted(InitiativeUsage{Today: InitiativePerChatPerDay + 1}) {
		t.Fatalf("счёт сверх лимита не считается исчерпанным")
	}
}

func TestIsInitiativeKind(t *testing.T) {
	for _, kind := range []string{"reply_later", "check_in", "share"} {
		if !IsInitiativeKind(kind) {
			t.Fatalf("%q должен считаться инициативой (§14.3)", kind)
		}
	}
	for _, kind := range []string{"explore", "", "answer"} {
		if IsInitiativeKind(kind) {
			t.Fatalf("%q не инициатива", kind)
		}
	}
}
