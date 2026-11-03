// Юнит-тесты repair-подсказки и следа невалидного ответа (21.7): ход 108
// ушёл в turn_skipped без сырого ответа модели — разбирать было нечего.
package character

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRepairHintAnswerWithoutMessages — общий шаблон остаётся, а при
// «answer без сообщений» подсказка прямо говорит, что делать вместо пустого
// answer (react/ignore).
func TestRepairHintAnswerWithoutMessages(t *testing.T) {
	perr := errors.New("turnresult: answer требует от 1 до 4 сообщений")
	hint := repairHint(perr)
	for _, want := range []string{
		"Твой последний ответ не прошёл валидацию",
		"answer требует от 1 до 4 сообщений",
		"decision=answer требует непустой messages с текстом",
		"decision=react с эмодзи или ignore",
	} {
		if !strings.Contains(hint, want) {
			t.Fatalf("в repair-подсказке нет %q:\n%s", want, hint)
		}
	}
	// Другая ошибка не получает подсказку про непустой answer.
	other := repairHint(errors.New("turnresult: react требует reaction (эмодзи)"))
	if strings.Contains(other, "непустой messages") {
		t.Fatalf("подсказка про answer приклеена к чужой ошибке:\n%s", other)
	}
	if !strings.Contains(other, "Верни строго один JSON-объект по схеме TurnResult") {
		t.Fatalf("общий шаблон repair потерян:\n%s", other)
	}
}

// TestInvalidResultSkipReasonKeepsRawOutput — причина пропуска несёт сырые
// тела первой и repair-попыток, каждое в пределах 2000 рун.
func TestInvalidResultSkipReasonKeepsRawOutput(t *testing.T) {
	first := `{"decision":"answer","messages":[]}`
	repair := `{"decision":"react"}`
	e := &invalidResultError{
		err:    errors.New("turnresult: answer требует от 1 до 4 сообщений"),
		first:  first,
		repair: repair,
	}
	reason := e.skipReason()
	for _, want := range []string{
		"answer требует",
		"попытка 1",
		first,
		"ответ на repair",
		repair,
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("в причине пропуска нет %q:\n%s", want, reason)
		}
	}

	long := strings.Repeat("я", invalidOutputRunes+10)
	if n := utf8.RuneCountInString(truncateRawOutput(long)); n != invalidOutputRunes {
		t.Fatalf("сырой ответ обрезан до %d рун, хочу %d", n, invalidOutputRunes)
	}
	if got := truncateRawOutput("короткий"); got != "короткий" {
		t.Fatalf("короткий ответ изменён: %q", got)
	}
}
