// Package migrations содержит начальную схему PostgreSQL (спека §18).
//
// SQL-файлы встраиваются в бинарник (go:embed), поэтому один бинарник
// сам поднимает схему на чистой базе — без внешних инструментов миграции.
package migrations

import "embed"

// FS — встроенные SQL-миграции: 000001_extensions.sql … 000015_stickers.sql.
// Применяются раннером internal/runtime.Migrate по порядку имён.
//
//go:embed *.sql
var FS embed.FS
