// Изоляция паник фона (пункт 16.2): Scheduler.Run крутится в собственной
// горутине, где паника без recover убивает весь процесс — вместе с приёмом и
// доставкой. Здесь три пояса: такт под recover (паника — ERROR в лог, цикл
// продолжается следующим тактом), и каждая задача (намерение, эпизод
// консолидации) под собственным recover — паника одной задачи становится
// ошибкой этой задачи (повтор по §12.1), не прерывая остальные задачи такта.
package background

import (
	"context"
	"fmt"
	"runtime/debug"
)

// tickGuarded — один такт под recover: паника пишется в лог ERROR со стеком,
// цикл Run продолжает следующим тактом.
func (s *Scheduler) tickGuarded(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logError("паника такта планировщика фона (такт пропущен, цикл продолжается): %v\n%s", r, debug.Stack())
		}
	}()
	if err := s.Tick(ctx); err != nil {
		s.logError("такт планировщика фона не удался: %v", err)
	}
}

// runIntentionGuarded — исполнение одного намерения под recover: паника —
// ошибка ЭТОЙ задачи (возврат в pending по §12.1), такт продолжается.
func (s *Scheduler) runIntentionGuarded(ctx context.Context, it Intention) (out intentionOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника исполнения: %v", r)
			s.logError("паника исполнения намерения %d (задача изолирована, такт продолжается): %v\n%s",
				it.ID, r, debug.Stack())
		}
	}()
	return s.runIntention(ctx, it)
}

// runConsolidationGuarded — консолидация одного эпизода под recover: паника —
// ошибка этой задачи (эпизод не помечен, повтор на следующем такте).
func (s *Scheduler) runConsolidationGuarded(ctx context.Context, ep Episode) (out consolidationOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника исполнения: %v", r)
			s.logError("паника консолидации эпизода чата %d (задача изолирована, такт продолжается): %v\n%s",
				ep.ChatID, r, debug.Stack())
		}
	}()
	return s.runConsolidation(ctx, ep)
}
