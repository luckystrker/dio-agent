// Юнит-проверка 18.3 (без БД): recovery-чтение журнала (stateOfEvents)
// ревалидирует сохранённый model_output текущими правилами §11.2 — повреждённая
// или записанная по старым правилам запись помечается OutputInvalid (ход
// получит терминальный skip), валидная разбирается с чисткой инертных полей.
package character

import (
	"encoding/json"
	"testing"

	"github.com/luckystrker/ago/events"
)

func modelOutputEvent(t *testing.T, result string) events.Event {
	t.Helper()
	return ModelOutput{Result: json.RawMessage(result)}
}

func TestStateOfEventsRevalidatesModelOutput(t *testing.T) {
	cases := []struct {
		name        string
		result      string
		wantInvalid bool
		wantOutput  bool
	}{
		{
			name:        "битый JSON",
			result:      `не json вообще`,
			wantInvalid: true,
		},
		{
			// До 17.8 писалось и применялось молча; теперь apply разыменовывает
			// DeferHours (apply.go) — запись обязана считаться невалидной.
			name:        "старые правила: defer без defer_hours",
			result:      `{"decision":"defer"}`,
			wantInvalid: true,
		},
		{
			name:        "answer без сообщений",
			result:      `{"decision":"answer"}`,
			wantInvalid: true,
		},
		{
			name:       "валидный answer",
			result:     `{"decision":"answer","messages":["ок"],"reason":"тест"}`,
			wantOutput: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := stateOfEvents([]events.Event{modelOutputEvent(t, tc.result)})
			if st.OutputInvalid != tc.wantInvalid {
				t.Fatalf("OutputInvalid = %v, хочу %v (reason=%q)", st.OutputInvalid, tc.wantInvalid, st.OutputInvalidReason)
			}
			if (st.Output != nil) != tc.wantOutput {
				t.Fatalf("Output = %v, хочу presence=%v", st.Output, tc.wantOutput)
			}
		})
	}
}

// Инертные поля чистятся и на recovery-пути (17.8): defer_hours у не-defer
// решения — не ошибка валидации, а мусор без эффекта; применение получает
// очищенный результат.
func TestStateOfEventsStripsInertOnRecovery(t *testing.T) {
	st := stateOfEvents([]events.Event{
		modelOutputEvent(t, `{"decision":"ignore","defer_hours":5,"reason":"тишина"}`),
	})
	if st.OutputInvalid {
		t.Fatalf("инертное противоречие уронило recovery-валидацию: %q", st.OutputInvalidReason)
	}
	if st.Output == nil || st.Output.DeferHours != nil {
		t.Fatalf("defer_hours не вычищен на recovery-пути: %+v", st.Output)
	}
}

// Поведение при нескольких model_output в потоке (например, после ремонта):
// последнее валидное побеждает, более ранняя невалидная запись не калечит
// восстановление.
func TestStateOfEventsLastOutputWins(t *testing.T) {
	st := stateOfEvents([]events.Event{
		modelOutputEvent(t, `{"decision":"defer"}`),               // старая/битая запись
		modelOutputEvent(t, `{"decision":"ignore","reason":"т"}`), // валидная
	})
	if st.OutputInvalid {
		t.Fatalf("поздняя валидная запись не затёрла раннюю невалидную: %q", st.OutputInvalidReason)
	}
	if st.Output == nil || st.Output.Decision != DecisionIgnore {
		t.Fatalf("Output = %+v, хочу последний (ignore)", st.Output)
	}
}

// 29.1: turn_failed — терминальное состояние журнала. Recovery читает его
// как провал (не как «модель ещё не отвечала») и не запускает новую генерацию
// даже при наличии валидного model_output в потоке.
func TestStateOfEventsFailedIsTerminal(t *testing.T) {
	const reason = "превышен лимит попыток (3): сбой применения"
	st := stateOfEvents([]events.Event{
		modelOutputEvent(t, `{"decision":"ignore","reason":"т"}`),
		TurnFailed{TurnID: 7, Phase: "worker", Reason: reason},
	})
	if !st.Failed {
		t.Fatal("turn_failed не помечен терминальным провалом")
	}
	if st.FailReason != reason {
		t.Fatalf("причина провала %q, хочу %q", st.FailReason, reason)
	}
	if st.Skipped {
		t.Fatal("провал смешан с обычным пропуском: это разные терминальные статусы")
	}
	if (TurnFailed{}).EventType() != EventTypeTurnFailed {
		t.Fatalf("тип события провала %q, хочу %q", (TurnFailed{}).EventType(), EventTypeTurnFailed)
	}
}
