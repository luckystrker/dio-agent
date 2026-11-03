// Prefilter (§6.3, этап 30): дешёвые правила до модели.
//
// В личке прямое общение — событие всегда идёт в ход. В группе на входе
// поддержанное событие остаётся pending (буфер копит пачку своим окном), а
// судьбу пачки решает дешёвый фильтр при её закрытии (group.go, 30.1):
// прямое обращение, вовлечённость (30.2), имя бота или «цепляющая тема» —
// ход; иначе journal_only. Так эмбеддинг темы не делается в ingest
// синхронно, а бурст группы не теряет контекст до решения.
//
// Неподдержанные updates фиксируются journal_only: offset не застревает.
package telegram

// IngestStatus — статус записи social_events на входе (§6.1):
// pending — событие пойдёт в буфер и (возможно) ход; journal_only — только
// журнал и недавняя история.
type IngestStatus string

const (
	StatusPending     IngestStatus = "pending"
	StatusJournalOnly IngestStatus = "journal_only"
)

// DecideIngestStatus решает судьбу события до модели (§6.3): поддержанное
// событие становится pending и попадает в буфер; пассивный шум группы
// отбрасывается позже, при закрытии пачки (30.1), когда видна вся порция.
func DecideIngestStatus(event NormalizedEvent) IngestStatus {
	if !event.Supported {
		return StatusJournalOnly
	}
	return StatusPending
}
