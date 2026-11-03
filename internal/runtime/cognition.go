// Семафор когниции (§5.2): одновременно выполняется не более одного
// модельного задания — прямой ход worker'а, инициатива, exploration или
// консолидация фона. Без общего замка фон и worker думали параллельно:
// два ApplyTurn, двойной расход бюджета и инициатива поверх свежего ответа.
package runtime

import "context"

// CognitionLock — семафор ёмкостью 1.
type CognitionLock struct {
	ch chan struct{}
}

// NewCognitionLock строит семафор.
func NewCognitionLock() *CognitionLock {
	return &CognitionLock{ch: make(chan struct{}, 1)}
}

// Acquire берёт семафор до отмены ctx. nil-замок — no-op (тесты и сборки без
// общей когниции). Возвращённый release идемпотентен.
func (l *CognitionLock) Acquire(ctx context.Context) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	select {
	case l.ch <- struct{}{}:
		var once bool
		return func() {
			if once {
				return
			}
			once = true
			<-l.ch
		}, true
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
		var once bool
		return func() {
			if once {
				return
			}
			once = true
			<-l.ch
		}, true
	default:
		return nil, false
	}
}
