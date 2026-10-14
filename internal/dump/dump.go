// Package dump renders tables as restorable MariaDB SQL.
package dump

import (
	"database/sql"
	"io"
	"strconv"

	"autodump/internal/source"
)

// Options limits the size of each multi-row INSERT statement.
// A zero or negative limit means unlimited.
type Options struct {
	InsertBatchRows  int
	InsertBatchBytes int
}

// SessionSQL are the session settings a dump file starts with. Importers that
// execute statements directly must run them on the same connection first.
var SessionSQL = []string{
	"/*!40101 SET NAMES utf8mb4 */",
	"SET time_zone='+00:00'",
	"SET FOREIGN_KEY_CHECKS=0",
	"SET UNIQUE_CHECKS=0",
	"SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO'",
}

// WriteFileHeader writes the session settings every dump file starts with.
func WriteFileHeader(w io.Writer) error {
	b := []byte("-- autodump\n")
	for _, s := range SessionSQL {
		b = append(b, s...)
		b = append(b, ";\n"...)
	}
	_, err := w.Write(b)
	return err
}

// WriteFileFooter restores checks and writes the completion marker line.
func WriteFileFooter(w io.Writer, tables int, rows int64) error {
	b := []byte("SET FOREIGN_KEY_CHECKS=1;\nSET UNIQUE_CHECKS=1;\n-- autodump complete: ")
	b = strconv.AppendInt(b, int64(tables), 10)
	b = append(b, " tables, "...)
	b = strconv.AppendInt(b, rows, 10)
	b = append(b, " rows\n"...)
	_, err := w.Write(b)
	return err
}

// WriteTable writes DROP, the DDL and the table's rows as batched INSERTs.
// stream must call its RowFunc once per row and return its errors.
//
// Statements end with ";\n" exactly at the end of a Write call and nowhere
// else at a Write boundary, so a writer can execute them one by one.
func WriteTable(w io.Writer, table, ddl string, stream func(source.RowFunc) error, opts Options) (int64, error) {
	tw := newTableWriter(w, table, opts)
	if err := tw.begin(ddl); err != nil {
		return 0, err
	}
	var names, types []string
	err := stream(func(cols []*sql.ColumnType, row []sql.RawBytes) error {
		if names == nil {
			names = make([]string, len(cols))
			types = make([]string, len(cols))
			for i, c := range cols {
				names[i], types[i] = c.Name(), c.DatabaseTypeName()
			}
		}
		return tw.row(names, types, row)
	})
	if err != nil {
		return tw.rows, err
	}
	return tw.rows, tw.end()
}

// tableWriter accumulates rows into one INSERT statement in a reused buffer.
type tableWriter struct {
	w        io.Writer
	table    string
	maxRows  int
	maxBytes int

	kinds  []kind
	prefix int    // length of "INSERT INTO ... VALUES " at the start of stmt
	stmt   []byte // current statement
	n      int    // rows in stmt
	rows   int64  // rows written in total
}

func newTableWriter(w io.Writer, table string, opts Options) *tableWriter {
	return &tableWriter{w: w, table: table, maxRows: opts.InsertBatchRows, maxBytes: opts.InsertBatchBytes}
}

func (t *tableWriter) begin(ddl string) error {
	b := appendIdent([]byte("DROP TABLE IF EXISTS "), t.table)
	b = append(b, ";\n"...)
	if _, err := t.w.Write(b); err != nil {
		return err
	}
	_, err := t.w.Write(append([]byte(ddl), ";\n"...))
	return err
}

// row appends one row; names and types are only read on the first call.
func (t *tableWriter) row(names, types []string, row []sql.RawBytes) error {
	if t.kinds == nil {
		t.initPrefix(names, types)
	}
	mark := len(t.stmt) // where this row's separator (or the first row) starts
	if t.n > 0 {
		t.stmt = append(t.stmt, ',')
	}
	start := len(t.stmt)
	t.stmt = append(t.stmt, '(')
	for i, v := range row {
		if i > 0 {
			t.stmt = append(t.stmt, ',')
		}
		t.stmt = appendKind(t.stmt, t.kinds[i], v)
	}
	t.stmt = append(t.stmt, ')')

	// Too big with this row: flush the previous rows and keep this one.
	if t.n > 0 && t.maxBytes > 0 && len(t.stmt)+2 > t.maxBytes {
		if err := t.write(t.stmt[:mark]); err != nil {
			return err
		}
		k := copy(t.stmt[t.prefix:], t.stmt[start:])
		t.stmt = t.stmt[:t.prefix+k]
		t.n = 0
	}
	t.n++
	t.rows++
	if t.maxRows > 0 && t.n >= t.maxRows {
		return t.flush()
	}
	return nil
}

func (t *tableWriter) initPrefix(names, types []string) {
	t.kinds = make([]kind, len(types))
	for i, typ := range types {
		t.kinds[i] = classify(typ)
	}
	b := append(make([]byte, 0, 64<<10), "INSERT INTO "...)
	b = appendIdent(b, t.table)
	b = append(b, " ("...)
	for i, name := range names {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendIdent(b, name)
	}
	b = append(b, ") VALUES "...)
	t.prefix = len(b)
	t.stmt = b
}

// write emits stmt (prefix + rows) terminated with ";\n".
func (t *tableWriter) write(stmt []byte) error {
	if _, err := t.w.Write(stmt); err != nil {
		return err
	}
	_, err := io.WriteString(t.w, ";\n")
	return err
}

// flush writes the pending statement, if any, and resets it to the prefix.
func (t *tableWriter) flush() error {
	if t.n == 0 {
		return nil
	}
	t.stmt = append(t.stmt, ";\n"...)
	_, err := t.w.Write(t.stmt)
	t.stmt = t.stmt[:t.prefix]
	t.n = 0
	return err
}

func (t *tableWriter) end() error { return t.flush() }
