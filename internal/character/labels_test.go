// Тесты меток сообщений (этап 37.2): короткие, стабильные, разбор терпимый к
// слабой модели, но не к произвольному мусору.
package character

import (
	"testing"
	"time"
)

func TestMessageLabel(t *testing.T) {
	if got := MessageLabel(123); got != "#m123" {
		t.Fatalf("метка = %q", got)
	}
	if got := MessageLabel(0); got != "" {
		t.Fatalf("у сообщения без Telegram id метки быть не должно: %q", got)
	}
}

func TestParseMessageLabel(t *testing.T) {
	for _, raw := range []string{"#m123", "m123", "#123", " M42 "} {
		id, ok := ParseMessageLabel(raw)
		if !ok {
			t.Fatalf("метка %q не разобрана", raw)
		}
		if id != 123 && id != 42 {
			t.Fatalf("метка %q → id %d", raw, id)
		}
	}
	for _, raw := range []string{"", "#m", "#m0", "#m-1", "#m12a", "привет", "#m12 34"} {
		if _, ok := ParseMessageLabel(raw); ok {
			t.Fatalf("мусор %q принят за метку", raw)
		}
	}
}

func TestParseTurnResultReplyTo(t *testing.T) {
	// Модель отвечает реплаем на сообщение истории: индекс 1-based, метка.
	ok, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["первое","второе"],"reply_to_by_message":[{"index":2,"ref":"#m77"}]}`))
	if err != nil {
		t.Fatalf("валидный reply_to_by_message отклонён: %v", err)
	}
	if len(ok.ReplyToByMessage) != 1 || ok.ReplyToByMessage[0].Index != 2 || ok.ReplyToByMessage[0].Ref != "#m77" {
		t.Fatalf("reply_to_by_message = %+v", ok.ReplyToByMessage)
	}

	// Индекс вне messages — мусор формы: строка без реплая, ход живёт.
	ok, err = ParseTurnResult([]byte(`{"decision":"answer","messages":["одно"],"reply_to_by_message":[{"index":5,"ref":"#m1"}]}`))
	if err != nil {
		t.Fatalf("индекс вне диапазона уронил ход: %v", err)
	}
	if len(ok.ReplyToByMessage) != 0 {
		t.Fatalf("индекс вне диапазона принят: %+v", ok.ReplyToByMessage)
	}

	// Реплай — только у сообщений ответа: react/ignore/defer поле не несут.
	for _, raw := range []string{
		`{"decision":"react","reaction":"🔥","reply_to_by_message":[{"index":1,"ref":"#m1"}]}`,
		`{"decision":"ignore","reply_to_by_message":[{"index":1,"ref":"#m1"}]}`,
		`{"decision":"defer","defer_hours":1,"reply_to_by_message":[{"index":1,"ref":"#m1"}]}`,
	} {
		parsed, err := ParseTurnResult([]byte(raw))
		if err != nil {
			t.Fatalf("%s: разбор не удался: %v", raw, err)
		}
		if len(parsed.ReplyToByMessage) != 0 {
			t.Fatalf("%s: реплай не обнулён: %+v", raw, parsed.ReplyToByMessage)
		}
	}

	// Пустая метка — мусор, строка отбрасывается; метка длиннее потолка — ошибка.
	ok, err = ParseTurnResult([]byte(`{"decision":"answer","messages":["x"],"reply_to_by_message":[{"index":1,"ref":"  "}]}`))
	if err != nil || len(ok.ReplyToByMessage) != 0 {
		t.Fatalf("пустая метка не отброшена: %+v, err=%v", ok.ReplyToByMessage, err)
	}
}

// TestBatchTextsCarryStableLabels — сообщения пачки получают ту же метку, что
// потом будет в истории: по ней модель отвечает реплаем на только что
// пришедшее сообщение.
func TestBatchTextsCarryStableLabels(t *testing.T) {
	batch := []BatchEvent{
		{EventID: 1, ActorID: 777, ActorName: "Коля", Text: "первое", MessageID: 501},
		{EventID: 2, ActorID: 0, Text: "служебное"},
	}
	got := batchTexts(batch)
	if got[0] != "#m501 первое" {
		t.Fatalf("метка пачки = %q", got[0])
	}
	if got[1] != "служебное" {
		t.Fatalf("сообщению без Telegram id метка не нужна: %q", got[1])
	}
	// Та же метка в истории — стабильна между ходами.
	line := renderHistoryLine(HistoryMessage{Role: "user", Content: "первое", Author: "Коля", Label: "#m501",
		At: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}, time.UTC)
	if line != "[чт 05.11 12:00] #m501 Коля: первое" {
		t.Fatalf("строка истории = %q", line)
	}
}

// TestParseTurnResultStripsCopiedLabel — модель копирует строку истории
// целиком: служебная метка и автор срезаются, в чат уходит только текст.
func TestParseTurnResultStripsCopiedLabel(t *testing.T) {
	history := []HistoryMessage{{Role: "user", Author: "Коля", Content: "привет"}}
	got, err := ParseTurnResultWithHistory(
		[]byte(`{"decision":"answer","messages":["[пн 02.11 17:02] #m123 Коля: привет"]}`), history)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if got.Messages[0] != "привет" {
		t.Fatalf("скопированная метка/автор не срезаны: %q", got.Messages[0])
	}
	// Служебная метка без текста — пустое сообщение уходит в repair.
	if _, err := ParseTurnResult([]byte(`{"decision":"answer","messages":["#m123"]}`)); err == nil {
		t.Fatal("ответ из одной метки принят как сообщение")
	}
}
