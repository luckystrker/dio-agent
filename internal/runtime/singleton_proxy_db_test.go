//go:build db

// БД-допроверка 18.11 (приёмка): сценарий «замок-соединение мертво И сеть
// пропала» у разработчика проверен только наполовину — что во время сбоя
// доклада нет. Здесь проверяется вторая половина: когда сеть ВОССТАНОВИЛАСЬ,
// а замок-соединение так и не отвечает (замок реально снят сервером), потеря
// обязана быть доложена — «ждать сеть» не должно превратиться в «молчать
// вечно» (§5.1: без singleton-защиты работу не продолжаем).
package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgProxy — локальный TCP-прокси до Postgres: закрытие слушающего сокета
// имитирует сетевой сбой для НОВЫХ соединений (до БД не достучаться), не
// трогая сам сервер и уже установленные соединения.
type pgProxy struct {
	addr      string // 127.0.0.1:порт слушающего сокета
	dsn       string // DSN через прокси
	upstream  string // host:port настоящего Postgres
	listeners []net.Listener
}

func newPGProxy(t *testing.T, dsn string) *pgProxy {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("DSN не разобран: %v", err)
	}
	p := &pgProxy{upstream: net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("прокси не слушает: %v", err)
	}
	p.addr = ln.Addr().String()
	p.listeners = append(p.listeners, ln)
	serveProxy(ln, p.upstream)
	t.Cleanup(p.stop)
	p.dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		cfg.User, cfg.Password, p.addr, cfg.Database)
	return p
}

// serveProxy принимает соединения и пробрасывает байты в обе стороны.
func serveProxy(ln net.Listener, upstream string) {
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go pipeProxy(client, upstream)
		}
	}()
}

func pipeProxy(client net.Conn, upstream string) {
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
		_ = client.Close()
	}()
	_, _ = io.Copy(client, server)
	_ = server.Close()
	_ = client.Close()
}

// stopAccepting закрывает слушающий сокет: установленные соединения живут,
// новые — connection refused (сеть «пропала» для новых соединений).
func (p *pgProxy) stopAccepting(t *testing.T) {
	t.Helper()
	for _, ln := range p.listeners {
		_ = ln.Close()
	}
	p.listeners = nil
}

// startAccepting снова слушает тот же адрес (сеть «восстановилась»).
func (p *pgProxy) startAccepting(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatalf("прокси не поднялся повторно: %v", err)
	}
	p.listeners = append(p.listeners, ln)
	serveProxy(ln, p.upstream)
}

func (p *pgProxy) stop() {
	for _, ln := range p.listeners {
		_ = ln.Close()
	}
	p.listeners = nil
}

// TestSingletonLossReportedAfterNetworkRecovers — замок-соединение оборвано
// сервером, сеть до БД пропала: молчим и ждём; сеть вернулась (БД отвечает
// новому соединению), замок-соединение по-прежнему мертво — потеря доложена.
func TestSingletonLossReportedAfterNetworkRecovers(t *testing.T) {
	dsn := singletonTestDSN(t)
	ctx := context.Background()

	proxy := newPGProxy(t, dsn)
	single, err := AcquireSingleton(ctx, proxy.dsn)
	if err != nil {
		t.Fatalf("замок через прокси не взят: %v", err)
	}
	lost := make(chan string, 1)
	single.OnLost(func(reason string) { lost <- reason })
	single.StartWatch(80 * time.Millisecond)
	t.Cleanup(func() { _ = single.Release(ctx) })

	// Замок-соединение мертво: сервер оборвал backend.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin-соединение не открыто: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_terminate_backend($1)", single.PID()); err != nil {
		t.Fatalf("backend не оборван: %v", err)
	}

	// Сеть «пропала»: новые соединения до БД не проходят.
	proxy.stopAccepting(t)

	// ~10 тиков: хватит на три неудачи Ping и несколько проверок сети —
	// доклада быть не должно, наблюдение продолжает ждать.
	select {
	case reason := <-lost:
		t.Fatalf("сетевой сбой доложен как потеря замка: %s", reason)
	case <-time.After(800 * time.Millisecond):
	}
	select {
	case <-single.watchDone:
		t.Fatalf("наблюдение остановилось при сетевом сбое — должно ждать сеть")
	default:
	}

	// Сеть вернулась: БД отвечает новому соединению, замок-соединение мертво —
	// потеря подтверждена и доложена.
	proxy.startAccepting(t)
	select {
	case reason := <-lost:
		if reason == "" {
			t.Fatalf("пустая причина потери")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("потеря не доложена после восстановления сети — процесс продолжил бы работу без singleton-защиты (§5.1)")
	}
}
