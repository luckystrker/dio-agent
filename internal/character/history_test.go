// Юнит-тесты истории (18.12): FitHistory — тем же результатом окна/бюджета,
// но одним проходом с конца, без аллокации на каждое сообщение (вставка в
// начало среза копировала буфер O(n²) раз).
package character

import (
	"reflect"
	"testing"
)

// TestFitHistoryNoPerMessageAllocations — линейный проход не выделяет память
// на каждое сообщение: раньше каждая вставка в начало среза выделяла и
// копировала новый буфер (1000 сообщений — 1000 аллокаций).
func TestFitHistoryNoPerMessageAllocations(t *testing.T) {
	const n = 1000
	messages := make([]HistoryMessage, n)
	for i := range messages {
		messages[i] = HistoryMessage{Role: "user", Content: "х"}
	}
	// Бюджет с запасом: всё окно влезает — худший случай для вставки в начало.
	allocs := testing.AllocsPerRun(10, func() { FitHistory(messages, n) })
	if allocs > n/10 {
		t.Fatalf("FitHistory аллоцирует на каждое сообщение (%.0f allocs на %d сообщений) — квадратичное копирование вставкой в начало", allocs, n)
	}
}

// TestFitHistorySameWindowBudget — вручную прорешанные случаи окна/бюджета:
// результат — всегда суффикс входа, первыми выпадают старые, самое свежее
// живёт всегда, объём считается рунами, а не байтами.
func TestFitHistorySameWindowBudget(t *testing.T) {
	cases := []struct {
		name     string
		messages []HistoryMessage
		maxChars int
		want     []HistoryMessage
	}{
		{
			// Размеры 5/3/4, бюджет 8: свежее(4)+среднее(3)=7 влезает, старое(5) — нет.
			name: "бюджет выталкивает только самое старое",
			messages: []HistoryMessage{
				{Role: "user", Content: "12345"},
				{Role: "assistant", Content: "abc"},
				{Role: "user", Content: "дыня"},
			},
			maxChars: 8,
			want: []HistoryMessage{
				{Role: "assistant", Content: "abc"},
				{Role: "user", Content: "дыня"},
			},
		},
		{
			// «ыы»=2 руны (4 байта), «ыыы»=3 руны (6 байт): бюджет 5 руны влезает
			// обе, байтовая арифметика выкинула бы старую.
			name: "объём считается рунами, не байтами",
			messages: []HistoryMessage{
				{Role: "user", Content: "ыы"},
				{Role: "assistant", Content: "ыыы"},
			},
			maxChars: 5,
			want: []HistoryMessage{
				{Role: "user", Content: "ыы"},
				{Role: "assistant", Content: "ыыы"},
			},
		},
		{
			name:     "пустая история — пустой результат",
			messages: nil,
			maxChars: 100,
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FitHistory(tc.messages, tc.maxChars)
			if len(got) != len(tc.want) {
				t.Fatalf("осталось %d сообщений, хочу %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("сообщение[%d] = %+v, хочу %+v", i, got[i], tc.want[i])
				}
			}
			if len(tc.messages) > 0 && len(got) > 0 && !sameSuffix(tc.messages, got) {
				t.Fatalf("результат не суффикс входа с сохранением порядка: %+v", got)
			}
		})
	}
}

// sameSuffix: want — суффикс src (окно не меняет порядок и не пропускает
// сообщения в середине).
func sameSuffix(src, want []HistoryMessage) bool {
	if len(want) > len(src) {
		return false
	}
	return reflect.DeepEqual(src[len(src)-len(want):], want)
}
