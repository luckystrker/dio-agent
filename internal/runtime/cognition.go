// Семафор когниции (§5.2): одновременно выполняется не более одного
// модельного задания — прямой ход worker'а, инициатива, exploration или
// консолидация фона. Без общего замка фон и worker думали параллельно:
// двойной расход бюджета и инициатива поверх свежего ответа. Применение
// результата у фона идёт после освобождения семафора, но оно транзакционно
// и защищено CAS хода/намерения (§12.1, §14.3).
package runtime

import (
	"context"
	"sync"
)

// CognitionLock — семафор ёмкостью 1.
type CognitionLock struct {
	ch chan struct{}
}

// NewCognitionLock строит семафор.
func NewCognitionLock() *CognitionLock {
	return &CognitionLock{ch: make(chan struct{}, 1)}
}

// Acquire берёт семафор до отмены ctx. nil-замок — no-op (тесты и сборки без
// общей когниции). Возвращённый release идемпотентен (sync.Once).
func (l *CognitionLock) Acquire(ctx context.Context) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	select {
	case l.ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-l.ch }) }, true
	case <-ctx.Done():
		return nil, false
	}
}

// TryAcquire — неблокирующая попытка для фона: занято — фоновый шаг
// откладывается, не задерживая прямой ход.
func (l *CognitionLock) TryAcquire() (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	select {
	case l.ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-l.ch }) }, true
	default:
		return nil, false
	}
}
