// Тихие часы (§14.3, этап 22.4): расписание дня персоны помечает ночной блок
// тихим. В тихом блоке фон не исполняет инициативы — check_in/share/
// reply_later вне живого ответа переносятся на конец блока (notBefore), а не
// отменяются. Прямой ответ пользователю тишиной не блокируется: у него свой
// путь через когнитивный worker (§5.2) и гейт сюда не попадает. Consolidation
// и explore — внутренние действия, тишиной не откладываются.
package background

import "time"

// quietDeferral — момент, до которого инициатива ждёт в тихом блоке. Второе
// значение — «сейчас тихо и это инициативный вид». Расписания нет — false
// (прежнее поведение).
func (s *Scheduler) quietDeferral(it Intention) (time.Time, bool) {
	if !isInitiativeKind(it.Kind) {
		return time.Time{}, false
	}
	return s.persona.QuietUntil(s.now(), s.location)
}
