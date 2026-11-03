package runtime

import (
	"context"
	"testing"
	"time"
)

// TestCognitionLockSerializes — M1: два модельных задания не идут параллельно.
func TestCognitionLockSerializes(t *testing.T) {
	lock := NewCognitionLock()
	release, ok := lock.Acquire(context.Background())
	if !ok {
		t.Fatal("первый захват не удался")
	}

	acquired := make(chan struct{})
	go func() {
		second, ok := lock.Acquire(context.Background())
		if ok {
			close(acquired)
			second()
		}
	}()

	select {
	case <-acquired:
		t.Fatal("второй захват прошёл, пока первый держит семафор")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("после release второй захват не прошёл")
	}
}

// TestCognitionLockTryAcquire — фон не встаёт в очередь за прямым ходом.
func TestCognitionLockTryAcquire(t *testing.T) {
	lock := NewCognitionLock()
	release, ok := lock.Acquire(context.Background())
	if !ok {
		t.Fatal("захват не удался")
	}
	defer release()
	if _, ok := lock.TryAcquire(); ok {
		t.Fatal("TryAcquire прошёл при занятом семафоре")
	}
}

// TestCognitionLockAcquireCanceled — ожидание семафора отменяется контекстом.
func TestCognitionLockAcquireCanceled(t *testing.T) {
	lock := NewCognitionLock()
	release, ok := lock.Acquire(context.Background())
	if !ok {
		t.Fatal("захват не удался")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := lock.Acquire(ctx); ok {
		t.Fatal("захват при отменённом контексте не должен проходить")
	}
}
