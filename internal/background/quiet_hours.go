// Тихие часы (§14.3, этап 22.4): расписание дня персоны помечает ночной блок
// тихим. В тихом блоке фон не исполняет инициативы — check_in/share/
// reply_later вне живого ответа переносятся на конец блока (notBefore), а не
// отменяются. Прямой ответ пользователю тишиной не блокируется: у него свой
// путь через когнитивный worker (§5.2) и гейт сюда не попадает. Consolidation
// и explore — внутренние действия, тишиной не откладываются.
package background

import "time"

// quietPlan — что делать с намерением в тихий блок: ждать до Until или
// отменить, если срок истекает раньше конца тишины (claim такое намерение уже
// не возьмёт — честнее сказать это сразу). Нулевое значение — тишины нет.
type quietPlan struct {
	Defer  bool
	Cancel bool
	Until  time.Time
}

// quietDecision — решение по тишине для намерения: только инициативные виды
// (check_in/share/reply_later), только при действующем тихом блоке, только в
// поясе персонажа (AGENT_TIMEZONE).
func (s *Scheduler) quietDecision(it Intention) quietPlan {
	if !isInitiativeKind(it.Kind) {
		return quietPlan{}
	}
	until, quiet := s.persona.QuietUntil(s.now(), s.location)
	if !quiet {
		return quietPlan{}
	}
	if it.ExpiresAt != nil && !until.Before(*it.ExpiresAt) {
		return quietPlan{Cancel: true, Until: until}
	}
	return quietPlan{Defer: true, Until: until}
}
