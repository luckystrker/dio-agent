# Повторное независимое ревью этапа 40 (2026-11-06, второй проход)

Диапазон: `git diff main...bb/40-replay-reasoning-thr_gkstrwz2xx` (11 коммитов:
8 исходных + `830377c`, `4b607e6`, `ff21e26` — исправления по первому ревью).
Проверялось состояние ветки на `ff21e26ded2d87c360e09af9d0d2608c0f1882b1`
(main на момент проверки — `e0ce0d5`).

Только ревью — код не менялся. Две временные проб-тесты гонялись
в извлечённой копии (`git archive` в `/tmp`, не worktree ветки) и удалены
после прогона; в коммит входит только этот отчёт.

Метод: сборка/вет/юниты из `git archive` (чистый checkout без worktree-мусора);
end-to-end проба M1 (endpoint `?api_key=SECRET` + транспортная ошибка, userinfo);
батарея детектора (15 строк живого мусора 246–248 + 45 нормальных строк:
код, «по схеме», императив+JSON, капс, разговор о полях схемы);
точечные тесты m3–m10; `go vet`; полный `go test ./...`;
полный `scripts/test-db.sh` на ветке + контрольный прогон упавшего E2E на main;
сверка, что схема/tools/кэш префикса не тронуты.

Итог: **B0, M1, M2, m1–m10 — закрыты, подтверждено тестами. Новых BLOCKER/MAJOR
нет.** Остаток: 2 MINOR (m11 — остаточный риск query-ключа в replay-документации,
m12 — предсуществующий красный `TestMainLoop`, не регрессия ветки) + 2 INFO.
n3 (atomic.Pointer) — принят как обоснованно не тронутый.

## B0 — закрыт

`git ls-tree -r bb/40-replay-reasoning-thr_gkstrwz2xx -- cmd/agent/`:
`main.go`, `replay.go`, `replay_test.go` — закоммичены. `.gitignore` ветки:
`/agent`, `/agent.exe`, `/cmd/agent/agent`, `/cmd/agent/agent.exe`, `/dio-agent`
— все паттерны якорены, исходники не прячутся, бинарник в корне по-прежнему
игнорируется.

`git archive bb/40-replay-reasoning-thr_gkstrwz2xx | tar -x -C <tmp>` →
`go build ./...` exit 0, `go vet ./...` чист, `go test ./internal/modebug/
./internal/character/ ./cmd/agent/` зелёные. Подтверждено сборкой
закоммиченного состояния.

## M1 — закрыт

`record()` теперь правит текст ошибки через `redactErrorMessage` (точная замена
известных URL + `userinfoInTextRe` + `queryParamInTextRe` с единым списком
`sensitiveQueryParam` — тот же, что для URL). End-to-end проба (временный тест,
удалён): `Transport` с падающим `RoundTripper` (`*url.Error` с полным URL),
endpoint `https://gw.example/v1/chat/completions?api_key=SECRET123`,
`https://user:PASS123@gw.example/...`, `?key=SECRET123` → в файлах дампа нет
ни `SECRET123`, ни `PASS123`, ни `Bearer K` (заголовок тоже срезан). Репо-тесты
`TestTransportRedactsSecretInError`, `TestTransportRedactsUserinfoInError`,
`TestTransportRedactsSecretQuery`, `TestReplayerBrokenEndpointRedactsSecret` —
зелёные.

## M2, m1, m2 — закрыты

Батарея (временный тест, удалён): 15/15 строк живого мусора ловятся
(`,`, `}`, `#m1034/#m1035` в тексте, `messagesекафедра`, `mood_delta},`,
`reply_to_by_message:,`, `sticker: #3`, `Укажи JSON по схеме Turn`,
`TurnResult`, `верни JSON`, `JSON only`, `{"decision":"x"`, `Decision: answer`,
голое `messages`, `#m12` в тексте). 45/45 нормальных строк чистые:
`func main() {`, `if err != nil {`, `x = {a: 1}`, `return nil }`, `[[1],[2]]`,
многострочный сниппет, ```-блок, `json.Marshal(map[string]any{"reason": "x"})`,
`{"messages": []}`, `result := map[string]any{...}`, вся серия «по схеме» /
императив+JSON / `поправил TurnResult`, `TRUST ME`, `REASON: опоздал`,
`no reason to worry`, `See the notes, please`, `I have no memory of that`,
`confidence is high`, `sticker = "Нет"`, короткие реплики/смайлики, вся серия
критика-2 про поля (`у defer_hours максимум 168 часов`,
`поле "messages" — массив строк`, `формат: {"decision":"answer",...}` и т.д.).

## m3–m10 — закрыты (тестом)

- m3: `TestDumperPruneRemovesStaleTmp` ✓ (`.json.tmp` со своим префиксом чистится
  ретенцией; чужой JSON не трогается — `TestDumperPruneKeepsForeignJSON` ✓).
- m4: `TestDumperKeepsBothAttempts` ✓ (`bg-intention-42-1-call-1.json` +
  `-r2.json`, temp резервируется `O_EXCL`).
- m5: `TestNewDumperKeepsExistingDirPerms` ✓ (существующий каталог не chmod-ится,
  предупреждение в лог; зафиксировано и в RUNBOOK).
- m6: `TestReplayerRejectsBrokenEndpoint`, `TestReplayerRejectsForeignHost`,
  `TestReplayerRejectsSchemeDowngrade` ✓ (fail-closed + сверка схемы).
- m7: `TestLoadDumpRejectsTruncatedRequest` ✓ (+ `TestLoadDumpRejectsNullRequest`).
- m8: `TestDropGarbageMessagesClearsForwardOnReact` ✓ (react-деградация снимает
  `Forward` и реплаи).
- m9: `TestApplyVariantsWarnsPanelMissing` ✓ (предупреждение, в CLI печатается
  один раз на набор абляций).
- m10: `TestTransportHidesAuthLikeHeaders` ✓ (`api_key`/`secret`/`token` в списке;
  query-вариант закрыт отдельно через `sensitiveQueryParam`).

## n1–n4

n1 (формулировка про нормализацию JSON), n2 (мёртвое условие убрано),
n4 (комментарий о цене 2 МиБ) — закрыты. n3 (`Transport.Dumper` без
atomic.Pointer) — **принят как обоснованно не тронутый**: установка один раз
на старте (`app.go:163`, под `installMu`), тесты последовательны; гонка
возможна только при concurrent `InstallDefaultTransport` с живым трафиком,
чего в проде нет. `-race` в этом окружении недоступен (нет gcc) — проверено
чтением + `go vet` чист.

## Нового не сломано; кэш/схема/tools не тронуты

- `go test ./...` (юнит): все пакеты ok.
- В диффе (38 файлов) нет `turn_result_schema.go`, промпт-шаблона, `persona.md`,
  memory/tools, `tool_choice`-проводки, `go.mod` — схема провода, набор
  инструментов и префикс стабильны (проверено списком файлов, exit 1 у grep).
- `scripts/test-db.sh` полностью на ветке: `character`, `background`, `memory`,
  `modebug`, `runtime`, `telegram`, `worker` и др. — ok. Единственный красный:
  `internal/app` `TestMainLoop` (шаг «stance эпизода 1 применён и инициатива
  доставлена», 30 с) — **тот же тест падает так же на чистом main**
  (архив main без кода 40.x, прогон в изоляции: 31.4 с, та же строка).
  Вывод: предсуществующее/окруженческое, не регрессия исправлений 40
  (см. m12 ниже).

## Находки повторного прохода

### MINOR

#### m11. Документация replay для query-ключа оставляет ручную правку URL
`docs/RUNBOOK.md` (§10, ветка): дамп от endpoint с ключом только в query
replay не проигрывает — оператор правит `url` в файле вручную.
`internal/modebug/replay.go`, `docs/RUNBOOK.md`.

Ручная правка секрета в файле с приватной перепиской — новый шанс утечки
(копия файла, shell-history, забытый REDACTED-обратный путь). Дешёвый фикс:
флаг `--api-key-env`/`--key <src>` (подстановка ключа в query из окружения
при отправке, без записи в файл) либо явный отказ replay с текстом вместо
ручной правки. Сейчас задокументировано честно — поэтому MINOR, не MAJOR.
Не подтверждено пробой (чтение + отсутствие теста на этот путь).

#### m12. `TestMainLoop` красный и на main (предсуществующее, не блокер 40)
`internal/app/e2e_main_loop_db_test.go:196`.

Полный `scripts/test-db.sh`: всё зелёное, кроме `TestMainLoop` (30 с на
«stance + инициатива»). Контрольный прогон того же теста на архиве main
(без кода 40.x) падает идентично — регрессией ветки не является
(перекликается с бэклог-заметкой «TestMainLoop зависит от тихого блока
персоны»). Фикс — вне этапа 40; фиксирую, чтобы красный гейт не приписали
ветке. Подтверждено двумя прогонами (ветка в общем прогоне + в изоляции;
main в изоляции).

### INFO (не находки, зафиксировано сознательно)

- **i1.** Голое `messages` / `MESSAGES` (сообщение из одного имени поля) —
  по-прежнему брак (правило «кроме имён полей речи нет»). Осознанный
  компромисс: реплика из одного служебного слова неотличима от формата.
- **i2.** Остаточный ложно-отрицательный по дизайну критика-2: любое «живое»
  слово (≥3 букв) вне имён полей снимает подозрение — `mood_delta}, блин`
  уже не брак. Цена принята исполнителем ради нулевых ложных срабатываний
  на разговоре о полях; repair до этого всё равно уже был один.
- **i3.** Английская жалоба модели (ход 247) детектором не ловится — как и
  было задумано (семантический мусор — в replay-отладку, не в страховку).

## Вердикт

**Можно вливать.** B0, M1, M2, m1–m10 закрыты и подтверждены тестами/пробами;
новых BLOCKER/MAJOR нет; схема/tools/кэш не тронуты; БД-гейт зелёный везде,
кроме предсуществующего красного `TestMainLoop`, который падает и на main.
m11 — кандидат в бэклог/этап 41, не блокер. n3 — принят с обоснованием.
