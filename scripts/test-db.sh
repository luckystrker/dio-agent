#!/usr/bin/env bash
# БД-тесты (go test -tags db) на эфемерном PostgreSQL 17 + pgvector.
#
# Зачем: боевой user-space кластер на 5433 (dio-postgres.service, база dio_go)
# тестам не место — другой пароль, и тесты не должны трогать живую базу.
#
# Что делает:
#   1) поднимает приватный кластер PG17 на свободном порту в каталоге на диске
#      (по умолчанию $HOME/.cache/dio-test-pg/<pid>; /tmp — tmpfs и переполняется);
#   2) initdb с локалью C.utf8 (иначе lower() кириллицы зависит от ICU), trust
#      только на 127.0.0.1, старт с listen_addresses=127.0.0.1 и unix-сокетом;
#   3) создаёт базу dio и расширение vector;
#   4) экспортирует TEST_DATABASE_URL и запускает go test -tags db -count=1;
#   5) по выходу (в т.ч. Ctrl-C/ошибка/падение тестов) останавливает и удаляет
#      кластер через trap.
#
# Порты PG — переменными с дефолтами:
#   PG_BIN       каталог бинарников PG17 (initdb, pg_ctl, psql)
#   PG_LIB       каталог с libpq.so.5 для LD_LIBRARY_PATH
#   TEST_PG_ROOT каталог для эфемерных кластеров (на диске, не /tmp)
#   TEST_PG_PORT конкретный порт (по умолчанию — первый свободный)
#
#   TEST_DB_TAGS  теги go build (по умолчанию db; эвалы — "db,eval")
#
# Использование:
#   scripts/test-db.sh                          # все пакеты (./...)
#   scripts/test-db.sh ./internal/app/          # один пакет
#   scripts/test-db.sh ./internal/app/ -run TestMainLoop -v
#   TEST_DB_TAGS="db,eval" scripts/test-db.sh ./internal/app/ -run TestEvalSuiteOffline
#
# 5433/dio_go не трогается никогда: скрипт работает только со своим портом.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PG_BIN="${PG_BIN:-$HOME/dio-stack/pgsql/usr/lib/postgresql/17/bin}"
PG_LIB="${PG_LIB:-$HOME/dio-stack/pgsql/usr/lib/x86_64-linux-gnu}"
TEST_PG_ROOT="${TEST_PG_ROOT:-$HOME/.cache/dio-test-pg}"

if [[ ! -x "$PG_BIN/initdb" || ! -x "$PG_BIN/pg_ctl" || ! -x "$PG_BIN/psql" ]]; then
	echo "test-db: не найден PostgreSQL 17 в PG_BIN=$PG_BIN" >&2
	echo "test-db: укажи каталог бинарников, например PG_BIN=/usr/lib/postgresql/17/bin" >&2
	exit 2
fi

export LD_LIBRARY_PATH="$PG_LIB${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export PATH="$PG_BIN:$PATH"

mkdir -p "$TEST_PG_ROOT"
CLUSTER_DIR="$TEST_PG_ROOT/$$"
DATA_DIR="$CLUSTER_DIR/data"
SOCKET_DIR="$CLUSTER_DIR/run"
LOG_FILE="$CLUSTER_DIR/postgres.log"

cleanup() {
	local code=$?
	trap - EXIT INT TERM
	# Журнал упавшего прогона сохраняем до удаления кластера: иначе причина
	# (не поднялся initdb/pg_ctl, упали тесты) исчезает вместе с каталогом.
	if (( code != 0 )) && [[ -f "$LOG_FILE" ]]; then
		cp -f "$LOG_FILE" "$TEST_PG_ROOT/last-failure.log" 2>/dev/null || true
	fi
	if [[ -f "$DATA_DIR/postmaster.pid" ]]; then
		"$PG_BIN/pg_ctl" -D "$DATA_DIR" -m immediate stop >/dev/null 2>&1 || true
	fi
	# Кластер эфемерный: после остановки удаляем целиком. Если остановка не
	# удалась — файлы всё равно наши собственные, вреда от них нет.
	rm -rf "$CLUSTER_DIR"
	if (( code != 0 )); then
		echo "test-db: журнал упавшего прогона: $TEST_PG_ROOT/last-failure.log" >&2
	fi
	exit "$code"
}
trap cleanup EXIT INT TERM

# Свободный порт: 20000–59999. Занятость проверяем bind'ом через python3 —
# между проверкой и стартом кластера гонка возможна, но pg_ctl упадёт громко,
# а не тихо переиспользует чужой сервер.
free_port() {
	if [[ -n "${TEST_PG_PORT:-}" ]]; then
		echo "$TEST_PG_PORT"
		return
	fi
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

PORT="$(free_port)"

mkdir -p "$DATA_DIR" "$SOCKET_DIR"

"$PG_BIN/initdb" -D "$DATA_DIR" \
	--locale=C.utf8 --encoding=UTF8 \
	--auth-local=trust --auth-host=trust \
	-U dio >"$CLUSTER_DIR/initdb.log" 2>&1 || {
	echo "test-db: initdb не удался ($CLUSTER_DIR/initdb.log)" >&2
	exit 1
}

# trust только на 127.0.0.1, unix-сокет — в приватном каталоге кластера.
"$PG_BIN/pg_ctl" -D "$DATA_DIR" -l "$LOG_FILE" -w -t 60 \
	-o "-p $PORT -k $SOCKET_DIR -c listen_addresses=127.0.0.1" start >/dev/null || {
	echo "test-db: кластер не запустился ($LOG_FILE)" >&2
	exit 1
}

"$PG_BIN/psql" -h 127.0.0.1 -p "$PORT" -U dio -d postgres -v ON_ERROR_STOP=1 \
	-c "CREATE DATABASE dio" \
	-c "CREATE EXTENSION IF NOT EXISTS vector" >/dev/null

export TEST_DATABASE_URL="postgres://dio@127.0.0.1:$PORT/dio?sslmode=disable"

echo "test-db: PostgreSQL 17 на 127.0.0.1:$PORT ($DATA_DIR)" >&2

cd "$ROOT"
if (($# == 0)); then
	set -- ./...
fi
go test -tags "${TEST_DB_TAGS:-db}" -count=1 "$@"
