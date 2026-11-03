package runtime

import (
	"testing"
	"time"
)

// TestNextDay — граница суток бюджета: 00:05 UTC следующего дня от даты
// аргумента (решение 16.7; формула перенесена из фона без изменений).
func TestNextDay(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"полдень", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)},
		{"конец дня", time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC),
			time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)},
		{"переход года", time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC),
			time.Date(2027, 1, 1, 0, 5, 0, 0, time.UTC)},
		// M6: 01:00 по Москве — это ещё 22:00 UTC предыдущего дня; ждать нужно
		// ближайшую полночь UTC, а не сутки после московской даты.
		{"ночь по Москве", time.Date(2026, 11, 2, 1, 0, 0, 0, time.FixedZone("MSK", 3*60*60)),
			time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)},
		// 23:30 UTC 1 ноября, но уже 2 ноября по Москве — дата аргумента в
		// местном поясе не должна сдвигать границу на лишние сутки.
		{"поздний вечер UTC", time.Date(2026, 11, 2, 2, 30, 0, 0, time.FixedZone("MSK", 3*60*60)),
			time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextDay(tc.now); !got.Equal(tc.want) {
				t.Fatalf("NextDay(%v) = %v, хочу %v", tc.now, got, tc.want)
			}
		})
	}
}
