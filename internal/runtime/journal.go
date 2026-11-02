// Адаптер журнала: stream.Storage фреймворка ago поверх таблицы event_streams
// (§12.1, §18). Таблица append-only (запрещены UPDATE/DELETE триггером),
// UNIQUE(stream_id, seq) даёт однократность записи события, из которой выводится
// однократность применения хода (инвариант 4).
//
// Контракт по решению 2026-10-30 в спеке («Изменения решений», пункт 9.2):
// SetHistory — без удалений: хранимая история должна совпадать с предложенной
// по префиксу, вставляется только суффикс (обычные добавления проходят,
// переписывание середины или сокращение — ошибка ErrHistoryConflict);
// DropHistory для долговременного журнала — явная ошибка ErrDropForbidden.
// Compaction-мидлвари на долговременный журнал ходов не вешаются (§17).
//
// События сериализуются через events.MarshalEvent (дискриминатор @type внутри
// payload); колонка type дублирует EventType для проверок восстановления §12.2
// на уровне SQL («turn_started без model_output» и т.п.).
package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/luckystrker/ago/events"
	agostream "github.com/luckystrker/ago/stream"
)

// Ошибки адаптера журнала. Проверяются через errors.Is.
var (
	// ErrDropForbidden — DropHistory на долговременном журнале (решение 2026-10-30).
	ErrDropForbidden = errors.New("журнал долговременный: DropHistory запрещён (§12.1)")
	// ErrHistoryConflict — предложенная история не совпадает с хранимой по префиксу.
	ErrHistoryConflict = errors.New("история не совпадает с хранимой по префиксу")
	// ErrSequenceGap — обрыв последовательности seq в потоке (повреждение журнала).
	ErrSequenceGap = errors.New("обрыв последовательности в журнале")
	// ErrTypeMismatch — колонка type не совпадает с @type в payload.
	ErrTypeMismatch = errors.New("тип события в журнале не совпадает с payload")
)

// PgStreamStorage — реализация agostream.Storage на PostgreSQL.
//
// Интерфейс ago не принимает context.Context, поэтому контекст передаётся при
// создании: все операции идут в этом контексте (отмена при shutdown останавливает
// и незавершённые записи журнала). Внутренние варианты с контекстом
// (getHistory/setHistory/append) использует WAL фоновых задач (wal.go).
type PgStreamStorage struct {
	ctx  context.Context
	pool *Pool
}

// Интерфейс ago должен выполняться во время компиляции.
var _ agostream.Storage = (*PgStreamStorage)(nil)

// NewPgStreamStorage создаёт адаптер журнала поверх пула PostgreSQL.
func NewPgStreamStorage(ctx context.Context, pool *Pool) *PgStreamStorage {
	return &PgStreamStorage{ctx: ctx, pool: pool}
}

// SaveEvent дописывает одно событие в конец потока. Семантика как у
// MemoryStorage через History.Append: чтение хранимого + дозапись; под
// конкуренцией писателей одного потока второй получает ErrHistoryConflict —
// молчаливого расхождения нет.
func (s *PgStreamStorage) SaveEvent(streamID string, ev events.Event) error {
	if ev == nil {
		return fmt.Errorf("журнал: событие пустое, поток %q", streamID)
	}
	return s.append(s.ctx, streamID, ev)
}

// GetHistory перечитывает поток по порядку seq. Обрыв последовательности
// (пропущенный номер) — ошибка ErrSequenceGap: восстановление есть
// перечитывание журнала (§12.2), и молча пропустить шаг оно не должно.
func (s *PgStreamStorage) GetHistory(streamID string) ([]events.Event, error) {
	return s.getHistory(s.ctx, streamID)
}

// SetHistory сверяет предложенную историю с хранимой по префиксу и вставляет
// только суффикс — одной короткой транзакцией с блокировкой строк потока.
// Переписывание середины, сокращение и повтор уже записанного события:
// первые два — ErrHistoryConflict, последний — честный no-op (однократность
// записи, §12.1: повторный прогон обработки не создаёт новых событий).
func (s *PgStreamStorage) SetHistory(streamID string, evs []events.Event) error {
	return s.setHistory(s.ctx, streamID, evs)
}

// DropHistory запрещён: журнал — источник истины, восстановление есть
// перечитывание (§12.1, §12.2); compaction на долговременный журнал не вешается.
func (s *PgStreamStorage) DropHistory(streamID string) error {
	return fmt.Errorf("%w: поток %q", ErrDropForbidden, streamID)
}

// append дописывает одно событие в конец потока в данном контексте (WAL).
func (s *PgStreamStorage) append(ctx context.Context, streamID string, ev events.Event) error {
	stored, err := s.getHistory(ctx, streamID)
	if err != nil {
		return err
	}
	proposed := make([]events.Event, len(stored), len(stored)+1)
	copy(proposed, stored)
	proposed = append(proposed, ev)
	return s.setHistory(ctx, streamID, proposed)
}

// getHistory читает поток по порядку seq и декодирует события.
func (s *PgStreamStorage) getHistory(ctx context.Context, streamID string) ([]events.Event, error) {
	if streamID == "" {
		return nil, errors.New("журнал: пустой stream_id")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, type, payload
		FROM event_streams
		WHERE stream_id = $1
		ORDER BY seq`, streamID)
	if err != nil {
		return nil, fmt.Errorf("журнал: поток %q не читается: %w", streamID, err)
	}
	defer rows.Close()

	var seqs []int64
	var types []string
	var payloads [][]byte
	for rows.Next() {
		var (
			seq     int64
			typ     string
			payload []byte
		)
		if err := rows.Scan(&seq, &typ, &payload); err != nil {
			return nil, fmt.Errorf("журнал: поток %q: %w", streamID, err)
		}
		seqs = append(seqs, seq)
		types = append(types, typ)
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("журнал: поток %q: %w", streamID, err)
	}

	if err := checkContiguous(streamID, seqs); err != nil {
		return nil, err
	}

	out := make([]events.Event, len(payloads))
	for i, payload := range payloads {
		ev, err := events.UnmarshalEvent(payload)
		if err != nil {
			return nil, fmt.Errorf("журнал: поток %q, seq %d: %w", streamID, seqs[i], err)
		}
		if ev.EventType() != types[i] {
			return nil, fmt.Errorf("%w: поток %q, seq %d: колонка %q, в payload %q",
				ErrTypeMismatch, streamID, seqs[i], types[i], ev.EventType())
		}
		out[i] = ev
	}
	return out, nil
}

// SetHistoryTx — вариант setHistory внутри ЧУЖОЙ транзакции (§12.1: запись
// дельт, создание outbox rows и события deltas_applied + delivery_queued —
// одна транзакция PostgreSQL). Строки потока блокируются, префикс сверяется,
// вставляется только суффикс; транзакцию фиксирует вызывающий. Уже записанная
// история длиннее предложенной — ErrHistoryConflict (повторное применение
// отклоняется), совпадение — честный no-op.
func (s *PgStreamStorage) SetHistoryTx(ctx context.Context, tx pgx.Tx, streamID string, evs []events.Event) error {
	if streamID == "" {
		return errors.New("журнал: пустой stream_id")
	}
	types, payloads, err := marshalAll(streamID, evs)
	if err != nil {
		return err
	}
	stored, err := lockPrefix(ctx, tx, streamID, types, payloads)
	if err != nil {
		return err
	}
	return insertSuffix(ctx, tx, streamID, evs, stored)
}

// setHistory проверяет префикс и вставляет суффикс в одной транзакции.
func (s *PgStreamStorage) setHistory(ctx context.Context, streamID string, evs []events.Event) error {
	if streamID == "" {
		return errors.New("журнал: пустой stream_id")
	}

	// Маршалим всё предложенное заранее: сбой сериализации не должен оставлять
	// полузаписанный суффикс.
	types, payloads, err := marshalAll(streamID, evs)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("журнал: поток %q: транзакция не открыта: %w", streamID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stored, err := lockPrefix(ctx, tx, streamID, types, payloads)
	if err != nil {
		return err
	}
	if err := insertSuffix(ctx, tx, streamID, evs, stored); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// marshalAll сериализует предложенную историю (тип + payload каждого события).
func marshalAll(streamID string, evs []events.Event) ([]string, []string, error) {
	types := make([]string, len(evs))
	payloads := make([]string, len(evs))
	for i, ev := range evs {
		if ev == nil {
			return nil, nil, fmt.Errorf("журнал: поток %q: событие %d пустое", streamID, i)
		}
		raw, err := events.MarshalEvent(ev)
		if err != nil {
			return nil, nil, fmt.Errorf("журнал: поток %q: событие %d не сериализуется: %w", streamID, i, err)
		}
		types[i] = ev.EventType()
		payloads[i] = string(raw)
	}
	return types, payloads, nil
}

// lockPrefix блокирует строки потока и сверяет хранимый префикс с предложенным
// (по jsonb-семантике); возвращает число совпавших событий.
func lockPrefix(ctx context.Context, tx pgx.Tx, streamID string, types, payloads []string) (int, error) {
	// Блокируем строки потока до конца транзакции: писатели одного потока
	// выстраиваются в очередь, чтение префикса стабильно.
	rows, err := tx.Query(ctx, `
		SELECT seq
		FROM event_streams
		WHERE stream_id = $1
		ORDER BY seq
		FOR UPDATE`, streamID)
	if err != nil {
		return 0, fmt.Errorf("журнал: поток %q не читается: %w", streamID, err)
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return 0, fmt.Errorf("журнал: поток %q: %w", streamID, err)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("журнал: поток %q: %w", streamID, err)
	}

	if err := checkContiguous(streamID, seqs); err != nil {
		return 0, err
	}

	stored := len(seqs)
	if stored > len(types) {
		return 0, fmt.Errorf("%w: поток %q: хранится %d событий, предложено %d — сокращение истории",
			ErrHistoryConflict, streamID, stored, len(types))
	}

	// Префикс: сравнение по jsonb-семантике (порядок ключей и формат чисел не
	// значимы), на уровне SQL — без самодельного diff по JSON в Go.
	if stored > 0 {
		var firstBad int64
		err := tx.QueryRow(ctx, `
			SELECT coalesce(min(p.ord), 0)
			FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS p(type, payload, ord)
			JOIN event_streams es ON es.stream_id = $1 AND es.seq = p.ord
			WHERE es.type IS DISTINCT FROM p.type
			   OR es.payload IS DISTINCT FROM p.payload::jsonb`,
			streamID, types[:stored], payloads[:stored]).Scan(&firstBad)
		if err != nil {
			return 0, fmt.Errorf("журнал: поток %q: префикс не сверяется: %w", streamID, err)
		}
		if firstBad > 0 {
			return 0, fmt.Errorf("%w: поток %q: событие на позиции %d переписано",
				ErrHistoryConflict, streamID, firstBad)
		}
	}
	return stored, nil
}

// insertSuffix вставляет события после хранимого префикса (from — его длина).
func insertSuffix(ctx context.Context, tx pgx.Tx, streamID string, evs []events.Event, from int) error {
	types, payloads, err := marshalAll(streamID, evs)
	if err != nil {
		return err
	}
	if from >= len(evs) {
		return nil // суффикс пуст: повторная запись той же истории — no-op
	}
	suffixTypes := types[from:]
	suffixPayloads := payloads[from:]
	tag, err := tx.Exec(ctx, `
		INSERT INTO event_streams (stream_id, seq, type, payload)
		SELECT $1, $2 + p.ord, p.type, p.payload::jsonb
		FROM unnest($3::text[], $4::text[]) WITH ORDINALITY AS p(type, payload, ord)`,
		streamID, from, suffixTypes, suffixPayloads)
	if err != nil {
		// Гонка двух писателей на свежем потоке: проигравший упирается в
		// UNIQUE(stream_id, seq) — это тот же конфликт префикса.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: поток %q: %v (гонка писателей)", ErrHistoryConflict, streamID, pgErr.Detail)
		}
		return fmt.Errorf("журнал: поток %q: суффикс не записан: %w", streamID, err)
	}
	if tag.RowsAffected() != int64(len(suffixTypes)) {
		return fmt.Errorf("журнал: поток %q: записано %d строк, предложено %d",
			streamID, tag.RowsAffected(), len(suffixTypes))
	}
	return nil
}

// checkContiguous требует, чтобы номера событий потока были 1..n без пропусков:
// обрыв виден при чтении, а не молча пропускается (требование 9.2).
func checkContiguous(streamID string, seqs []int64) error {
	for i, seq := range seqs {
		if seq != int64(i+1) {
			return fmt.Errorf("%w: поток %q: ожидался seq %d, найден %d",
				ErrSequenceGap, streamID, i+1, seq)
		}
	}
	return nil
}
