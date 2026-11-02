// Юнит-допроверка 18.12 (приёмка): переписанный FitHistory обязан давать
// ровно то же окно/бюджет, что и прежняя вставка-в-начало. Эталон ниже
// повторяет старую реализацию дословно (из диффа до правки) — сравнение идёт
// на прорешанных краях и на переборе сгенерированных случаев, включая
// многобайтовые руны и бюджеты меньше самого свежего сообщения.
package character

import (
	"testing"
	"unicode/utf8"
)

// fitHistoryPrepend — эталон ДО правки (O(n²)): самое свежее живёт всегда,
// старые добавляются, пока суммарный объём не превысит бюджет.
func fitHistoryPrepend(messages []HistoryMessage, maxChars int) []HistoryMessage {
	var out []HistoryMessage
	total := 0
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		size := utf8.RuneCountInString(message.Content)
		if len(out) > 0 && total+size > maxChars {
			break
		}
		total += size
		out = append([]HistoryMessage{message}, out...)
	}
	return out
}

// TestFitHistoryMatchesPrependReference — перебор: новый проход-с-конца
// эквивалентен старой вставке-в-начало на 300 детерминированных случаях
// (длины 0..11, размеры реплик 0..6 рун вперемешку ASCII/кириллица, бюджеты
// от отрицательных до заведомо избыточных).
func TestFitHistoryMatchesPrependReference(t *testing.T) {
	next := uint32(1)
	rnd := func(n uint32) uint32 {
		next = next*1664525 + 1013904223
		return next % n
	}
	for caseIdx := 0; caseIdx < 300; caseIdx++ {
		n := int(rnd(12))
		msgs := make([]HistoryMessage, n)
		for i := range msgs {
			sz := int(rnd(7))
			runes := make([]rune, sz)
			for j := range runes {
				if rnd(2) == 0 {
					runes[j] = rune('а' + rnd(3))
				} else {
					runes[j] = 'x'
				}
			}
			msgs[i] = HistoryMessage{Role: "user", Content: string(runes)}
		}
		maxChars := int(rnd(20)) - 2

		want := fitHistoryPrepend(msgs, maxChars)
		got := FitHistory(msgs, maxChars)

		if len(got) != len(want) {
			t.Fatalf("кейс %d (n=%d, maxChars=%d): осталось %d сообщений, эталон даёт %d: %+v",
				caseIdx, n, maxChars, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("кейс %d (n=%d, maxChars=%d): сообщение[%d] = %+v, эталон %+v",
					caseIdx, n, maxChars, i, got[i], want[i])
			}
		}
	}
}

// TestFitHistoryEdges — края бюджета вручную: свежее живёт всегда (даже когда
// само больше бюджета), граница сравнения строгая (ровно в бюджет — влезает),
// нулевой и отрицательный бюджет не выбрасывают самое свежее.
func TestFitHistoryEdges(t *testing.T) {
	cases := []struct {
		name     string
		messages []HistoryMessage
		maxChars int
		want     []string
	}{
		{
			name: "бюджет меньше самого свежего — свежее всё равно живёт",
			messages: []HistoryMessage{
				{Role: "user", Content: "12345"},
				{Role: "assistant", Content: "abc"},
			},
			maxChars: 2,
			want:     []string{"abc"},
		},
		{
			name: "ровно в бюджет — ничего не выбрасывается (граница строгая)",
			messages: []HistoryMessage{
				{Role: "user", Content: "12345"},
				{Role: "assistant", Content: "abc"},
			},
			maxChars: 8,
			want:     []string{"12345", "abc"},
		},
		{
			name: "нулевой бюджет — только самое свежее",
			messages: []HistoryMessage{
				{Role: "user", Content: "старое"},
				{Role: "assistant", Content: "новое"},
			},
			maxChars: 0,
			want:     []string{"новое"},
		},
		{
			name: "отрицательный бюджет — только самое свежее",
			messages: []HistoryMessage{
				{Role: "user", Content: "старое"},
				{Role: "assistant", Content: "новое"},
			},
			maxChars: -1,
			want:     []string{"новое"},
		},
		{
			name:     "одно сообщение — живёт при любом бюджете",
			messages: []HistoryMessage{{Role: "user", Content: "одиночка"}},
			maxChars: 0,
			want:     []string{"одиночка"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FitHistory(tc.messages, tc.maxChars)
			if len(got) != len(tc.want) {
				t.Fatalf("осталось %d сообщений (%+v), хочу %d: %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i].Content != tc.want[i] {
					t.Fatalf("сообщение[%d] = %q, хочу %q", i, got[i].Content, tc.want[i])
				}
			}
			// Полнота результата: то же, что давал эталон до правки.
			wantRef := fitHistoryPrepend(tc.messages, tc.maxChars)
			if len(wantRef) != len(got) {
				t.Fatalf("расхождение с эталоном: новый %d, эталон %d", len(got), len(wantRef))
			}
		})
	}
}
