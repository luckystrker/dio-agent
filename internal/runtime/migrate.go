// Раннер SQL-миграций (требование 9.1): таблица schema_migrations, применение
// по порядку имён, идемпотентно — повторный прогон ничего не меняет.
//
// Каждая миграция применяется целиком в одной транзакции: простым протоколом
// PostgreSQL в одном запросе уходит BEGIN; <sql>; INSERT INTO
// schema_migrations …; COMMIT — либо всё, либо ничего (философия «база —
// источник истины»). SQL-файлы встраиваются в бинарник (package migrations).
package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Migration — одна применяемая единица схемы.
type Migration struct {
	Name string // имя файла, например 000001_extensions.sql; оно же — ключ в schema_migrations
	SQL  string
}

// MigrationResult — итог прогона раннера.
type MigrationResult struct {
	Applied int // применено сейчас
	Skipped int // уже были применены ранее (идемпотентность)
}

// ListMigrations читает *.sql из fsys и возвращает их по порядку имён —
// префикс NNNNNN задаёт порядок применения.
func ListMigrations(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("миграции не читаются: %w", err)
	}
	sort.Strings(names)

	migrations := make([]Migration, 0, len(names))
	for _, name := range names {
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("миграция %s не читается: %w", name, err)
		}
		if strings.TrimSpace(string(sql)) == "" {
			return nil, fmt.Errorf("миграция %s пуста", name)
		}
		migrations = append(migrations, Migration{Name: name, SQL: string(sql)})
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("миграций не найдено")
	}
	return migrations, nil
}

// PendingMigrations отбирает ещё не применённые миграции.
func PendingMigrations(all []Migration, applied map[string]bool) []Migration {
	pending := make([]Migration, 0, len(all))
	for _, m := range all {
		if !applied[m.Name] {
			pending = append(pending, m)
		}
	}
	return pending
}

// Migrate применяет миграции к базе. Идемпотентно: второй прогон на той же
// базе применяет ноль миграций и не является ошибкой.
func Migrate(ctx context.Context, pool *Pool, fsys fs.FS) (MigrationResult, error) {
	all, err := ListMigrations(fsys)
	if err != nil {
		return MigrationResult{}, err
	}

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return MigrationResult{}, fmt.Errorf("schema_migrations не создана: %w", err)
	}

	applied := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("schema_migrations не читается: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return MigrationResult{}, fmt.Errorf("schema_migrations: %w", err)
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MigrationResult{}, fmt.Errorf("schema_migrations: %w", err)
	}

	pending := PendingMigrations(all, applied)
	result := MigrationResult{Skipped: len(all) - len(pending)}
	for _, m := range pending {
		if err := applyMigration(ctx, pool, m); err != nil {
			return result, fmt.Errorf("миграция %s не применена: %w", m.Name, err)
		}
		result.Applied++
	}
	return result, nil
}

// applyMigration применяет одну миграцию атомарно: BEGIN; <sql>; отметка в
// schema_migrations; COMMIT — одним запросом простого протокола (PgConn.Exec),
// который умеет несколько операторов за раз, в отличие от расширенного.
func applyMigration(ctx context.Context, pool *Pool, m Migration) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("нет соединения: %w", err)
	}
	defer conn.Release()

	script := "BEGIN;\n" + m.SQL + "\nINSERT INTO schema_migrations (name) VALUES (" + quoteLiteral(m.Name) + ");\nCOMMIT;"
	if _, err := conn.Conn().PgConn().Exec(ctx, script).ReadAll(); err != nil {
		// Вернуть соединение в строй после оборванной транзакции.
		_, _ = conn.Conn().PgConn().Exec(ctx, "ROLLBACK").ReadAll()
		return err
	}
	return nil
}

// quoteLiteral экранирует строку для SQL-литерала. Имя миграции приходит из
// встраиваемого FS (наши собственные файлы), но экранируем честно.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
