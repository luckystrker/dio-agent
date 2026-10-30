// Ошибки модуля памяти. Сопоставление через errors.Is.
package memory

import "errors"

var (
	// ErrInvalidScope — скоуп не соответствует формату §9.1 (public,
	// internal, chat:<id>).
	ErrInvalidScope = errors.New("memory: недопустимый scope")

	// ErrNotFound — объект памяти не найден или не виден аудитории
	// (невидимые объекты не отличимы от несуществующих: само существование
	// приватной записи не раскрывается — §9.2).
	ErrNotFound = errors.New("memory: не найдено")

	// ErrSupersedeScope — пересмотр с переносом записи в другой скоуп:
	// перенос приватной записи в чужую аудиторию запрещён (§9.1).
	ErrSupersedeScope = errors.New("memory: смена scope при пересмотре запрещена")

	// ErrSupersedeFork — повторный пересмотр уже заменённой/отозванной
	// записи: плодились бы «форки» текущей версии одного мнения (§10.3).
	ErrSupersedeFork = errors.New("memory: пересмотр уже заменённой записи запрещён")

	// ErrInvalidCandidate — кандидат памяти не прошёл проверку runtime
	// (тип, длина, диапазон, неизвестная ссылка) — §10.1.
	ErrInvalidCandidate = errors.New("memory: кандидат не прошёл проверку")

	// ErrSemanticUnavailable — векторизатор не настроен (EMBEDDING_*), семантическая
	// ветка поиска недоступна; full-text остаётся рабочим (§10.4 п.4).
	ErrSemanticUnavailable = errors.New("memory: семантический поиск недоступен (векторизатор не настроен)")
)
