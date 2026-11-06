// Package migrations содержит начальную схему PostgreSQL (спека §18).
//
// SQL-файлы встраиваются в бинарник (go:embed), поэтому один бинарник
// сам поднимает схему на чистой базе — без внешних инструментов миграции.
package migrations

import (
	"embed"
	"io/fs"
	"strings"
)

// FS — встроенные SQL-миграции: 000001_extensions.sql … 000025_avatar_invisible_reported.sql.
// Применяются раннером internal/runtime.Migrate по порядку имён.
//
//go:embed *.sql
var FS embed.FS

// Count — число SQL-миграций в наборе: тесты сверяют «применено всё» без
// ручного счётчика, который отстаёт при каждой новой миграции.
func Count(fsys fs.FS) int {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}
