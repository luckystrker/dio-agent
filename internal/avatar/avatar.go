// Аватарки собеседников (34.2): состояние по (пользователь, скоуп чата) и
// очередь фоновой проверки. Решение — в спеке (2026-11-04): смотрим при
// первом разговоре с человеком и при смене file_unique_id, но не чаще раза в
// сутки; описание — отдельный дешёвый модельный вызов, результат — запись
// памяти в скоупе чата §9.2. Обычные ходы аватарку не качают.
package avatar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Интервалы проверки (начальные настраиваемые значения).
const (
	// CheckInterval — повторная проверка смены аватарки: не чаще раза в сутки
	// на человека в скоупе чата.
	CheckInterval = 24 * time.Hour
	// RetryInterval — пауза после транзиентного сбоя (сеть/модель): не
	// молотим каждым тактом, но и не ждём сутки.
	RetryInterval = time.Hour
	// Lease — захват строки планировщиком: второй такт не берёт ту же проверку.
	Lease = 15 * time.Minute
	// DescriptionLimit — потолок описания одной фразой (руны).
	DescriptionLimit = 200
)

// DB — минимум запросов: пул PostgreSQL и транзакция.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Check — одна проверка аватарки из очереди.
type Check struct {
	UserID       int64
	ScopeID      string
	ChatID       int64
	ActorName    string
	FileUniqueID string // последний увиденный file_unique_id; пусто — не было
	Visible      bool
	Description  string // последнее записанное описание; пусто — не было
	// InvisibleReported — «аватарки не видно» уже сообщали хотя бы раз (m26):
	// повтор и переход visible→invisible не пишут память снова.
	InvisibleReported bool
}

// Enqueue ставит проверку в очередь для первого разговора в скоупе и после
// истечения суточного окна. Повторные ходы до срока очередь не трогают —
// аватарка не качается на обычных ходах.
func Enqueue(ctx context.Context, db DB, userID int64, scopeID string, chatID int64, actorName string) error {
	if userID == 0 {
		return nil // анонимный актор: аватарки нет
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO person_avatars (user_id, scope_id, chat_id, actor_name, next_check_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), now())
		ON CONFLICT (user_id, scope_id) DO UPDATE
		  SET chat_id = EXCLUDED.chat_id,
		      actor_name = COALESCE(EXCLUDED.actor_name, person_avatars.actor_name),
		      next_check_at = now()
		  WHERE person_avatars.next_check_at <= now()`,
		userID, scopeID, chatID, actorName); err != nil {
		return fmt.Errorf("аватарки: проверка пользователя %d не поставлена: %w", userID, err)
	}
	return nil
}

// ClaimDue захватывает до limit проверок (FOR UPDATE SKIP LOCKED) и сдвигает
// next_check_at на Lease — второй такт не возьмёт ту же строку.
func ClaimDue(ctx context.Context, db DB, limit int) ([]Check, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		WITH due AS (
		  SELECT user_id, scope_id FROM person_avatars
		  WHERE next_check_at <= now()
		  ORDER BY next_check_at, user_id, scope_id
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED
		)
		UPDATE person_avatars p
		SET next_check_at = now() + $2::bigint * interval '1 second'
		FROM due
		WHERE p.user_id = due.user_id AND p.scope_id = due.scope_id
		RETURNING p.user_id, p.scope_id, p.chat_id, COALESCE(p.actor_name, ''),
		          COALESCE(p.file_unique_id, ''), p.visible, COALESCE(p.description, ''),
		          p.invisible_reported`,
		limit, int64(Lease/time.Second))
	if err != nil {
		return nil, fmt.Errorf("аватарки: очередь проверок не читается: %w", err)
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.UserID, &c.ScopeID, &c.ChatID, &c.ActorName,
			&c.FileUniqueID, &c.Visible, &c.Description, &c.InvisibleReported); err != nil {
			return nil, fmt.Errorf("аватарки: строка проверки не читается: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("аватарки: очередь проверок не дочитана: %w", err)
	}
	return out, nil
}

// Complete фиксирует успешную проверку: увиденный file_unique_id, видимость,
// описание и маркер «не видно уже сообщали» (m26); следующая проверка — через
// сутки.
func Complete(ctx context.Context, db DB, userID int64, scopeID, fileUniqueID string, visible bool, description string, invisibleReported bool) error {
	if _, err := db.Exec(ctx, `
		UPDATE person_avatars
		SET file_unique_id = NULLIF($3, ''), visible = $4, description = NULLIF($5, ''),
		    invisible_reported = $6,
		    checked_at = now(), next_check_at = now() + $7::bigint * interval '1 second'
		WHERE user_id = $1 AND scope_id = $2`,
		userID, scopeID, fileUniqueID, visible, description, invisibleReported,
		int64(CheckInterval/time.Second)); err != nil {
		return fmt.Errorf("аватарки: результат пользователя %d не записан: %w", userID, err)
	}
	return nil
}

// Fail откладывает проверку после транзиентного сбоя: аватарка не должна
// молотить моделью/сетью каждый такт.
func Fail(ctx context.Context, db DB, userID int64, scopeID string) error {
	if _, err := db.Exec(ctx, `
		UPDATE person_avatars
		SET next_check_at = now() + $3::bigint * interval '1 second'
		WHERE user_id = $1 AND scope_id = $2`,
		userID, scopeID, int64(RetryInterval/time.Second)); err != nil {
		return fmt.Errorf("аватарки: отложенная проверка пользователя %d не записана: %w", userID, err)
	}
	return nil
}

// DescriptionText — текст записи памяти о видимой аватарке: «у <имя> на
// аватарке …». Пустое имя — «у собеседника».
func DescriptionText(name, description string) string {
	return "у " + displayName(name) + " на аватарке " + normalizeDescription(description)
}

// InvisibleText — «аватарки не видно» пишется один раз: приватность или
// отсутствие аватарки не повод ретраить каждый ход.
func InvisibleText(name string) string {
	return "у " + displayName(name) + " не видно аватарки"
}

func displayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "собеседника"
	}
	return name
}

// normalizeDescription — одна строка без переносов и лишних кавычек; потолок
// DescriptionLimit рун.
func normalizeDescription(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "«»\"'")
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > DescriptionLimit {
		text = string(runes[:DescriptionLimit])
	}
	if text == "" {
		return "что-то не разобрать"
	}
	return text
}
