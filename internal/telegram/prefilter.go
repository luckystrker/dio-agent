// Prefilter (§6.3): дешёвые правила до модели — process или journal_only.
// Прямой запрос не отбрасывается никогда; низкосигнальный шум группы
// остаётся в недавней истории, но не вызывает обязательный LLM и не
// становится durable memory автоматически. Поведение — src/telegram/prefilter.ts.
package telegram

// IngestStatus — статус записи social_events на входе (§6.1):
// pending — событие пойдёт в буфер и ход; journal_only — только журнал.
type IngestStatus string

const (
	StatusPending     IngestStatus = "pending"
	StatusJournalOnly IngestStatus = "journal_only"
)

// DecideIngestStatus решает судьбу события до модели (§6.3):
// личка и reply/mention боту/command в группе — прямое обращение (pending);
// пассивный шум группы и правки — журнал без обязательного LLM. Reply на
// чужое сообщение и @чужой_ник в группе — не обращение (M8); без известной
// идентичности бота (dry-run, старые тесты) действуют прежние правила.
func DecideIngestStatus(event NormalizedEvent) IngestStatus {
	if !event.Supported {
		return StatusJournalOnly
	}
	if event.ChatType == ChatPrivate {
		// Правка не создаёт новый ход и повторный платный ответ (minor 13):
		// текст правки в историю не попадает, пока нет отдельной обработки
		// edit/delete (бэклог §19).
		if event.Kind == KindEdited {
			return StatusJournalOnly
		}
		return StatusPending
	}
	switch event.Kind {
	case KindCommand:
		return StatusPending
	case KindReply:
		if !event.BotKnown || event.ReplyToBot {
			return StatusPending
		}
	case KindMention:
		if !event.BotKnown || event.MentionBot {
			return StatusPending
		}
	}
	return StatusJournalOnly
}
