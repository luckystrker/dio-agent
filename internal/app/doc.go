// Package app собирает приложение из модулей и управляет жизненным циклом
// процесса (§5.1, §5.3): singleton §5.1, startup (БД → миграции → singleton →
// персона → recovery → polling/worker/scheduler/delivery), graceful shutdown
// (остановить приём → ограниченно дождаться текущего → закрыть соединения) и
// жёсткая остановка: recovery обязан работать и после SIGKILL, graceful
// shutdown не является условием корректности (§5.3).
//
// Точки внедрения Options (Model, Telegram, Embedder, Now, размеры окон) —
// для тестов и альтернативных конфигураций; продакшн собирает всё из
// runtime.Config.
package app
