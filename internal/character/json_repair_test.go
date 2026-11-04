// Юнит-тесты этапа 33.3: модель иногда кладёт в строковый литерал сырые
// управляющие символы (живой случай bg:intention:97 — «invalid character '\t'
// in string»). До разбора экранируем их аккуратным сканером (не regexp по
// всему тексту); оборванный ответ (unexpected EOF) — repair-подсказка «верни
// короче».
package character

import (
	"bytes"
	"errors"
	"testing"
)

func TestEscapeJSONControlCharsInStrings(t *testing.T) {
	raw := []byte("{\"decision\":\"answer\",\"messages\":[\"привет\tкак\tдела\nи\rвсё\"]}")
	fixed := EscapeJSONControlChars(raw)
	if bytes.ContainsAny(fixed, "\t\n\r") {
		t.Fatalf("сырые управляющие символы остались: %q", fixed)
	}
	r, err := ParseTurnResult(fixed)
	if err != nil {
		t.Fatalf("экранированный ответ не разобран: %v", err)
	}
	if want := "привет\tкак\tдела\nи\rвсё"; r.Messages[0] != want {
		t.Fatalf("текст сообщения повреждён: %q, хочу %q", r.Messages[0], want)
	}
}

func TestParseTurnResultEscapesRawTab(t *testing.T) {
	// Точный живой случай: сырой таб внутри строки делал ответ невалидным.
	r, err := ParseTurnResult([]byte("{\"decision\":\"answer\",\"messages\":[\"привет\tкак дела\"]}"))
	if err != nil {
		t.Fatalf("сырой таб не пережит: %v", err)
	}
	if r.Messages[0] != "привет\tкак дела" {
		t.Fatalf("сообщение: %q", r.Messages[0])
	}
}

func TestEscapeJSONControlCharsKeepsValidJSON(t *testing.T) {
	// Уже корректные escape-последовательности и структура не меняются.
	for _, raw := range [][]byte{
		[]byte(`{"decision":"answer","messages":["a\nb"]}`),
		[]byte(`{"decision":"answer","messages":["кавычка \" и слэш \\"]}`),
		[]byte("{\n  \"decision\": \"ignore\"\n}"),
	} {
		if got := EscapeJSONControlChars(raw); !bytes.Equal(got, raw) {
			t.Fatalf("валидный JSON изменён:\nбыло  %q\nстало %q", raw, got)
		}
	}
}

func TestRepairHintTruncated(t *testing.T) {
	hint := RepairHint(errors.New("turnresult: unexpected EOF"), "TurnResult")
	for _, want := range []string{"оборвался", "короче"} {
		if !bytes.Contains([]byte(hint), []byte(want)) {
			t.Fatalf("подсказка для оборванного ответа не содержит %q: %q", want, hint)
		}
	}
	other := RepairHint(errors.New("turnresult: неизвестное decision"), "TurnResult")
	if bytes.Contains([]byte(other), []byte("оборвался")) {
		t.Fatalf("обычная ошибка получила подсказку про обрыв: %q", other)
	}
}
