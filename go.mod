module github.com/luckystrker/dio-agent

go 1.24

require (
	ago v0.0.0
	github.com/jackc/pgx/v5 v5.7.6
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.37.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/text v0.24.0 // indirect
)

// Фреймворк ago — вендорная копия (см. AGENTS.md, спека §17): module path «ago»
// не резолвится через прокси Go, поэтому подключён локально.
replace ago => ./third_party/ago
