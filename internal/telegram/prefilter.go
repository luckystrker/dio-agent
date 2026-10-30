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
// личка и reply/mention/command в группе — прямое обращение (pending);
// пассивный шум группы и правки — журнал без обязательного LLM.
func DecideIngestStatus(event NormalizedEvent) IngestStatus {
	if !event.Supported {
		return StatusJournalOnly
	}
	if event.ChatType == ChatPrivate {
		return StatusPending
	}
	switch event.Kind {
	case KindReply, KindMention, KindCommand:
		return StatusPending
	default:
		return StatusJournalOnly
	}
}
