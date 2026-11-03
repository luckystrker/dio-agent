//go:build db

// БД-тесты устойчивости наблюдения singleton (18.11): один неудачный Ping —
// ещё не потеря замка. Потеря докладывается после 3 подряд неудач и только
// если БД отвечает НОВОМУ соединению из того же DSN: замок-соединение мёртв
// при живой БД — замок потерян по-настоящему; БД недоступна целиком — сеть,
// процесс ждёт и не валится.
package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestSingletonLossNeedsThreeStrikes — обрыв backend: доклад о потере не раньше
// трёх подряд неудач Ping (интервал 150 мс ⇒ потеря не может быть доложена
// раньше ~375 мс), и не позже: 3 тика + проверка БД новым соединением (~30–50
// мс на локальной машине) укладываются в 580 мс — четвёртая неудача (порог
// больше трёх) уже не успевает (4 тика = 600 мс).
func TestSingletonLossNeedsThreeStrikes(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	single.StartWatch(150 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	// Разрыв со стороны СУБД сразу после старта наблюдения.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin-соединение не открыто: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", single.PID()); err != nil {
		t.Fatalf("backend не оборван: %v", err)
	}

	// Три неудачи — это три тика по 150 мс; доклад раньше ~375 мс — это
	// реакция на первую же неудачу (ложный рестарт от GC-паузы/микросбоя).
	select {
	case reason := <-lost:
		t.Fatalf("потеря доложена раньше трёх неудач: %s", reason)
	case <-time.After(375 * time.Millisecond):
	}

	// Верхняя граница: 375 + 205 = 580 мс от старта. Порог больше трёх дал
	// бы четвёртую неудачу не раньше 600 мс — за границей окна.
	select {
	case reason := <-lost:
		if reason == "" {
			t.Fatalf("пустая причина потери")
		}
	case <-time.After(205 * time.Millisecond):
		t.Fatalf("потеря не доложена за 580 мс от старта — порог больше трёх (лишний тик) или проверка БД зависла")
	}
}

// TestSingletonFailuresResetBySuccess — разрозненные неудачи не копятся:
// паттерн «неудача, неудача, успех» шесть раз подряд — ни разу три неудачи
// друг за другом, доклада быть не должно: каждый успешный Ping сбрасывает
// счётчик. Без сброса накопление разрозненных неудач даёт ложный fatal —
// ровно сценарий ТЗ 18.11 (GC-пауза/микросброс — не потеря).
func TestSingletonFailuresResetBySuccess(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })

	var mu sync.Mutex
	calls := 0          // номер Ping-а с начала наблюдения
	failAlways := false // фаза 2: сплошные неудачи
	pingErr := errors.New("ping: сбой (шов теста)")
	single.ping = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if failAlways || calls%3 != 0 {
			return pingErr
		}
		return nil
	}
	single.verify = func() bool { return true } // БД жива — решает только счётчик
	single.StartWatch(5 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	// Фаза 1: не меньше 18 Ping-ов паттерном. Граница фазы — сразу после
	// успешного Ping-а (счётчик неудач пуст); переключение на сплошные
	// неудачи — атомарно с чтением счётчика, чтобы между ними не проскочил
	// паттернный Ping.
	var before int
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		c := calls
		ready := c >= 18 && c%3 == 0
		if ready {
			failAlways = true
			before = c
		}
		mu.Unlock()
		if ready {
			break
		}
		select {
		case reason := <-lost:
			t.Fatalf("разрозненные неудачи (две через успех) накопились в ложный доклад: %s", reason)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("наблюдение не дошло до 18 Ping-ов за 5 с (сейчас %d) — тест ненадёжен", c)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Фаза 2: сплошные неудачи — контрольная точка порога: доклад ровно на
	// третьей подряд.
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatalf("три подряд неудачи не доложены за 5 с")
	}
	mu.Lock()
	total := calls
	mu.Unlock()
	if got := total - before; got != 3 {
		t.Fatalf("доклад на %d-й неудаче подряд, хочу ровно на 3-й", got)
	}
}

// TestSingletonReportsOnThirdConsecutiveFailure — доклад выходит ровно на
// третьей подряд неудаче Ping (порог задан ТЗ: 3 подряд): на второй рано,
// на четвёртой — поздно (задержка обнаружения потери на интервал).
func TestSingletonReportsOnThirdConsecutiveFailure(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })

	var mu sync.Mutex
	calls := 0
	single.ping = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return errors.New("ping: сбой (шов теста)")
	}
	single.verify = func() bool { return true }
	single.StartWatch(5 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	select {
	case reason := <-lost:
		if reason == "" {
			t.Fatalf("пустая причина потери")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("сплошные неудачи Ping не доложены за 5 с")
	}
	mu.Lock()
	c := calls
	mu.Unlock()
	if c != 3 {
		t.Fatalf("доклад на %d-й неудаче подряд, хочу ровно на 3-й (ТЗ: 3 подряд)", c)
	}
}

// TestSingletonNetworkOutageKeepsWaiting — замок-соединение мёртво, но и БД
// не отвечает новому соединению (сеть): потери НЕ докладываем, наблюдение
// продолжает работать и ждёт восстановления.
func TestSingletonNetworkOutageKeepsWaiting(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	// Проверочное соединение заведомо недоступно (порт 1): имитация сетевого
	// разрыва — до БД не достучаться и заново.
	single.dsn = "postgres://127.0.0.1:1/dio?sslmode=disable"
	single.StartWatch(50 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin-соединение не открыто: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", single.PID()); err != nil {
		t.Fatalf("backend не оборван: %v", err)
	}

	// 500 мс = ~10 тиков: хватит на три неудачи и несколько проверок сети —
	// ни одного доклада быть не должно.
	select {
	case reason := <-lost:
		t.Fatalf("сетевой сбой доложен как потеря замка: %s", reason)
	case <-time.After(500 * time.Millisecond):
	}

	// Наблюдение живо: цикл не вышел (watchDone не закрыт).
	select {
	case <-single.watchDone:
		t.Fatalf("наблюдение остановилось при сетевом сбое — процесс ждётся systemd, а должен ждать сеть")
	default:
	}
}

// TestSingletonNoFalseLossWhenHealthy — короткий интервал без обрывов: всё
// живо, ложных докладов нет, процесс продолжает работать.
func TestSingletonNoFalseLossWhenHealthy(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	single.StartWatch(25 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	select {
	case reason := <-lost:
		t.Fatalf("ложная потеря при живом соединении: %s", reason)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSingletonSlowPingIsNotLoss — таймаут Ping развязан с интервалом
// наблюдения: короткий интервал (как в E2E, 10–20 мс) не делает Ping короче
// самого интервала. Медленный, но успешный Ping (дольше тика, в пределах
// singletonProbeTimeout) — не потеря; иначе три тика «не уложился» с живой БД
// давали ложный fatal и останавливали процесс (флейк E2E под нагрузкой).
func TestSingletonSlowPingIsNotLoss(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	single, err := AcquireSingleton(ctx, dsn)
	if err != nil {
		t.Fatalf("замок не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	single.ping = func(ctx context.Context) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil // медленно, но соединение живо
		case <-ctx.Done():
			return ctx.Err() // короткий таймаут сделал бы из этого «неудачу»
		}
	}
	single.verify = func() bool { return true }
	single.StartWatch(10 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	// 400 мс = много тиков по 10 мс; при таймауте, равном интервалу, ложная
	// потеря была бы доложена на третьем тике (~30 мс).
	select {
	case reason := <-lost:
		t.Fatalf("медленный успешный Ping принят за потерю замка: %s", reason)
	case <-time.After(400 * time.Millisecond):
	}
}
