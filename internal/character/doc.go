// Package character — персонаж и ход (§4, §7, §11, §12).
//
// Модуль 9.4:
//   - persona-файлы §7.1 (persona_file.go): config/persona.md и
//     config/system-prompt.md — файлы-источники, парсер строгий, версия в БД
//     создаётся при отличии файла от активной записи (persona.go);
//   - mood §7.2 (mood.go): три параметра, детерминированное затухание к
//     baseline, ограниченные дельты;
//   - отношения §7.3 (relationships.go): familiarity/affinity/trust,
//     ограниченные изменения;
//   - базовый контекст §11.1 (context.go, history.go): persona+constraints
//     структурной секцией (§17), batch, mood, отношения, недавний разговор
//     обеих сторон с окном и потолком объёма;
//   - ход §11–12 (turn.go, apply.go, journal.go, events.go): запуск ago-агента
//     с инструментами памяти §10.4 (forced первый вызов на прямом обращении —
//     model.go), строгий TurnResult §11.2 с одним repair-retry §11.3, события
//     хода в event_streams §12.1, применение дельт + outbox + события одной
//     транзакцией, восстановление по журналу §12.2;
//   - middleware хода (middleware.go): бюджет §16, typing-пульс §13.3, лимит
//     обращений к инструментам §11.2.
package character
