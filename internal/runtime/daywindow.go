// Граница суток дневного бюджета (§16): пулы обнуляются в полночь UTC,
// отложенная работа повторяется после 00:05 (запас на полуночные maintenance).
// Общее место для фона (§14) и прямых ходов (worker §5.2): одна граница —
// один паритет откладывания (решение 16.7).
package runtime

import "time"

// NextDay — момент после полуночи UTC (00:05) следующего за now дня.
func NextDay(now time.Time) time.Time {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	return midnight.Add(5 * time.Minute)
}
