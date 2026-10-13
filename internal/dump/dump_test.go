package dump

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"autodump/internal/source"
)

// sql.ColumnType can't be built outside database/sql, so batching tests drive
// the internal tableWriter with explicit names and types.
func writeFake(t *testing.T, rows [][]sql.RawBytes, opts Options) (string, int64) {
	t.Helper()
	var out bytes.Buffer
	tw := newTableWriter(&out, "t`x", opts)
	if err := tw.begin("CREATE TABLE `t``x` (id int, s varchar(9))"); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := tw.row([]string{"id", "s`q"}, []string{"INT", "VARCHAR"}, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.end(); err != nil {
		t.Fatal(err)
	}
	return out.String(), tw.rows
}

func mkRows(n int) [][]sql.RawBytes {
	rows := make([][]sql.RawBytes, n)
	for i := range rows {
		rows[i] = []sql.RawBytes{sql.RawBytes(string(rune('0' + i%10))), sql.RawBytes("v")}
	}
	return rows
}

const (
	head = "DROP TABLE IF EXISTS `t``x`;\nCREATE TABLE `t``x` (id int, s varchar(9));\n"
	ins  = "INSERT INTO `t``x` (`id`,`s``q`) VALUES "
)

func TestWriteTableEmpty(t *testing.T) {
	got, n := writeFake(t, nil, Options{InsertBatchRows: 10, InsertBatchBytes: 1 << 20})
	if got != head || n != 0 {
		t.Fatalf("got %q rows %d", got, n)
	}
}

func TestWriteTableBatchByRows(t *testing.T) {
	got, n := writeFake(t, mkRows(5), Options{InsertBatchRows: 2, InsertBatchBytes: 1 << 20})
	want := head +
		ins + "(0,'v'),(1,'v');\n" +
		ins + "(2,'v'),(3,'v');\n" +
		ins + "(4,'v');\n"
	if got != want || n != 5 {
		t.Fatalf("got\n%s\nwant\n%s\nrows %d", got, want, n)
	}
}

func TestWriteTableBatchByBytes(t *testing.T) {
	// Exactly two rows fit: prefix + two rows + ";\n".
	limit := len(ins) + len("(0,'v'),(1,'v')") + len(";\n")
	got, n := writeFake(t, mkRows(5), Options{InsertBatchRows: 1000, InsertBatchBytes: limit})
	want := head +
		ins + "(0,'v'),(1,'v');\n" +
		ins + "(2,'v'),(3,'v');\n" +
		ins + "(4,'v');\n"
	if got != want || n != 5 {
		t.Fatalf("got\n%s\nwant\n%s\nrows %d", got, want, n)
	}
}

func TestWriteTableHugeRowAlone(t *testing.T) {
	rows := [][]sql.RawBytes{
		{sql.RawBytes("1"), sql.RawBytes("a")},
		{sql.RawBytes("2"), sql.RawBytes(strings.Repeat("x", 500))},
		{sql.RawBytes("3"), nil},
	}
	got, n := writeFake(t, rows, Options{InsertBatchRows: 1000, InsertBatchBytes: 100})
	want := head +
		ins + "(1,'a');\n" +
		ins + "(2,'" + strings.Repeat("x", 500) + "');\n" +
		ins + "(3,NULL);\n"
	if got != want || n != 3 {
		t.Fatalf("got\n%s\nwant\n%s\nrows %d", got, want, n)
	}
}

func TestWriteTableStreamError(t *testing.T) {
	boom := errors.New("boom")
	_, err := WriteTable(&bytes.Buffer{}, "t", "CREATE TABLE t (a int)",
		func(source.RowFunc) error { return boom }, Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteTableEmptyStream(t *testing.T) {
	var out bytes.Buffer
	n, err := WriteTable(&out, "document_1", "CREATE TABLE `document_1` (a int)",
		func(source.RowFunc) error { return nil }, Options{InsertBatchRows: 10, InsertBatchBytes: 100})
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	want := "DROP TABLE IF EXISTS `document_1`;\nCREATE TABLE `document_1` (a int);\n"
	if out.String() != want {
		t.Fatalf("got %q", out.String())
	}
}

func TestHeaderFooter(t *testing.T) {
	var out bytes.Buffer
	if err := WriteFileHeader(&out); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileFooter(&out, 3, 42); err != nil {
		t.Fatal(err)
	}
	want := "-- autodump\n/*!40101 SET NAMES utf8mb4 */;\nSET time_zone='+00:00';\n" +
		"SET FOREIGN_KEY_CHECKS=0;\nSET UNIQUE_CHECKS=0;\nSET SQL_MODE='NO_AUTO_VALUE_ON_ZERO';\n" +
		"SET FOREIGN_KEY_CHECKS=1;\nSET UNIQUE_CHECKS=1;\n-- autodump complete: 3 tables, 42 rows\n"
	if out.String() != want {
		t.Fatalf("got\n%s", out.String())
	}
}
