// Механика возврата соединения обёрткой Pool (18.5): pgxpool возвращает
// соединение в пул из внутренних poolRows/poolRow/Tx — они недоступны вне
// пакета, поэтому семантика воспроизведена здесь по образу pgxpool v5.7.6
// (pgxpool/rows.go, pgxpool/tx.go): Close/исчерпание строк, Scan одной
// строки, Commit/Rollback транзакции — идемпотентно (nil-защита).
package runtime

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errRows — строки в ошибочном состоянии (соединение не заимствовалось).
type errRows struct {
	err error
}

func (errRows) Close()                                       {}
func (e errRows) Err() error                                 { return e.err }
func (errRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (errRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (errRows) Next() bool                                   { return false }
func (e errRows) Scan(dest ...any) error                     { return e.err }
func (e errRows) Values() ([]any, error)                     { return nil, e.err }
func (e errRows) RawValues() [][]byte                        { return nil }
func (errRows) Conn() *pgx.Conn                              { return nil }

// errRow — одна строка в ошибочном состоянии.
type errRow struct {
	err error
}

func (e errRow) Scan(dest ...any) error { return e.err }

// poolRows — строки поверх заимствованного соединения: соединение
// возвращается по Close или исчерпанию (Next=false).
type poolRows struct {
	r pgx.Rows
	c *pgxpool.Conn
}

func (rows *poolRows) Close() {
	rows.r.Close()
	if rows.c != nil {
		rows.c.Release()
		rows.c = nil
	}
}

func (rows *poolRows) Err() error {
	return rows.r.Err()
}

func (rows *poolRows) CommandTag() pgconn.CommandTag {
	return rows.r.CommandTag()
}

func (rows *poolRows) FieldDescriptions() []pgconn.FieldDescription {
	return rows.r.FieldDescriptions()
}

func (rows *poolRows) Next() bool {
	n := rows.r.Next()
	if !n {
		rows.Close()
	}
	return n
}

func (rows *poolRows) Scan(dest ...any) error {
	err := rows.r.Scan(dest...)
	if err != nil {
		rows.Close()
	}
	return err
}

func (rows *poolRows) Values() ([]any, error) {
	values, err := rows.r.Values()
	if err != nil {
		rows.Close()
	}
	return values, err
}

func (rows *poolRows) RawValues() [][]byte {
	return rows.r.RawValues()
}

func (rows *poolRows) Conn() *pgx.Conn {
	return rows.r.Conn()
}

// poolRow — одна строка поверх заимствованного соединения: соединение
// возвращается по Scan (или панике до него).
type poolRow struct {
	r pgx.Row
	c *pgxpool.Conn
}

func (row *poolRow) Scan(dest ...any) error {
	panicked := true
	defer func() {
		if panicked && row.c != nil {
			row.c.Release()
			row.c = nil
		}
	}()
	err := row.r.Scan(dest...)
	panicked = false
	if row.c != nil {
		row.c.Release()
		row.c = nil
	}
	return err
}

// poolTx — транзакция поверх заимствованного соединения: Commit/Rollback
// возвращают соединение (повторный вызов после закрытия — ErrTxClosed,
// соединение не возвращается дважды).
type poolTx struct {
	pgx.Tx
	c *pgxpool.Conn
}

func (tx *poolTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if tx.c != nil {
		tx.c.Release()
		tx.c = nil
	}
	return err
}

func (tx *poolTx) Rollback(ctx context.Context) error {
	err := tx.Tx.Rollback(ctx)
	if tx.c != nil {
		tx.c.Release()
		tx.c = nil
	}
	return err
}
