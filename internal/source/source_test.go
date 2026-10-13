package source

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestDSN(t *testing.T) {
	dsn := DSN(ConnParams{Login: "u@x", Password: "p:a/s?s@w#rd", Host: "db.local:3306", Database: "docs"})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", dsn, err)
	}
	if cfg.User != "u@x" || cfg.Passwd != "p:a/s?s@w#rd" || cfg.Addr != "db.local:3306" || cfg.DBName != "docs" {
		t.Fatalf("round trip mismatch: %+v", cfg)
	}
	if cfg.ParseTime {
		t.Fatal("parseTime must be off")
	}
	if cfg.Params["time_zone"] != "'+00:00'" {
		t.Fatalf("time_zone = %q", cfg.Params["time_zone"])
	}
	if !strings.Contains(dsn, "charset=utf8mb4") || cfg.ReadTimeout != 10*time.Minute ||
		cfg.WriteTimeout != time.Minute || cfg.Timeout != 30*time.Second || !cfg.AllowNativePasswords {
		t.Fatalf("bad options in %q", dsn)
	}
}

func TestTableName(t *testing.T) {
	if got := TableName(42); got != "document_42" {
		t.Fatal(got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestIsTransient(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("x"), false},
		{driver.ErrBadConn, true},
		{mysql.ErrInvalidConn, true},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{&net.OpError{Op: "read", Err: timeoutErr{}}, true},
		{&mysql.MySQLError{Number: 1213}, true},
		{&mysql.MySQLError{Number: 1205}, true},
		{&mysql.MySQLError{Number: 1040}, true},
		{&mysql.MySQLError{Number: 2013}, true},
		{&mysql.MySQLError{Number: 1146}, false},
		{&mysql.MySQLError{Number: 1064}, false},
		{fmt.Errorf("%w: document_1", ErrNoSuchTable), false},
		{context.Canceled, false},
	}
	for _, tt := range tests {
		if got := IsTransient(tt.err); got != tt.want {
			t.Errorf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestTableErr(t *testing.T) {
	err := tableErr(&mysql.MySQLError{Number: 1146, Message: "gone"}, 7)
	if !errors.Is(err, ErrNoSuchTable) || !strings.Contains(err.Error(), "document_7") {
		t.Fatal(err)
	}
	other := &mysql.MySQLError{Number: 1064}
	if tableErr(other, 7) != other {
		t.Fatal("other errors must pass through")
	}
}

func TestSelectSQL(t *testing.T) {
	if got := selectSQL("document_1", []string{"id", "s"}, []string{"UNSIGNED BIGINT", "VARCHAR"}); got != "SELECT * FROM `document_1`" {
		t.Fatal(got)
	}
	got := selectSQL("document_1", []string{"id", "p`x"}, []string{"INT", "FLOAT"})
	if want := "SELECT `id`,`p``x`+0e0 AS `p``x` FROM `document_1`"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestDSNServerTimeZone(t *testing.T) {
	utc, _ := mysql.ParseDSN(DSN(ConnParams{Host: "h:1", Database: "d"}))
	if utc.Params["time_zone"] != "'+00:00'" {
		t.Fatalf("default DSN must force UTC, params=%v", utc.Params)
	}
	native, _ := mysql.ParseDSN(DSN(ConnParams{Host: "h:1", Database: "d", ServerTimeZone: true}))
	if _, ok := native.Params["time_zone"]; ok {
		t.Fatalf("ServerTimeZone DSN must not set time_zone, params=%v", native.Params)
	}
}
