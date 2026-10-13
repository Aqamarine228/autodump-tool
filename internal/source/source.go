// Package source reads the document index and document tables from MariaDB.
package source

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"xorm.io/xorm"
	xlog "xorm.io/xorm/log"
)

// ConnParams identifies a MariaDB database.
type ConnParams struct {
	Login, Password, Host, Database string
	// ServerTimeZone keeps the server's session time zone instead of forcing
	// UTC. The index connection needs it so NOW() matches how created_at is
	// stored; document dumps must stay in UTC so TIMESTAMP values round-trip.
	ServerTimeZone bool
}

// DSN builds a go-sql-driver DSN. parseTime stays off so temporal values
// arrive as the server's text, and the session runs in UTC.
func DSN(p ConnParams) string {
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = p.Host
	c.DBName = p.Database
	c.User = p.Login
	c.Passwd = p.Password
	c.ParseTime = false
	c.Loc = time.UTC
	c.AllowNativePasswords = true
	c.ReadTimeout = 10 * time.Minute
	c.WriteTimeout = time.Minute
	c.Timeout = 30 * time.Second
	c.Params = map[string]string{
		"charset": "utf8mb4",
		// A slow writer must not make the server drop a streaming SELECT.
		"net_write_timeout": "600",
	}
	if !p.ServerTimeZone {
		c.Params["time_zone"] = "'+00:00'"
	}
	return c.FormatDSN()
}

// Open connects to the database and verifies the connection.
func Open(p ConnParams) (*xorm.Engine, error) {
	eng, err := xorm.NewEngine("mysql", DSN(p))
	if err != nil {
		return nil, err
	}
	eng.ShowSQL(false)
	eng.SetLogLevel(xlog.LOG_OFF)
	eng.SetMaxOpenConns(8)
	eng.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := eng.PingContext(ctx); err != nil {
		eng.Close()
		return nil, err
	}
	return eng, nil
}

// Blocker is the first document that is not ready for export.
type Blocker struct {
	ID       uint64
	AgeHours int64
}

// Index reads the parsing_documents table.
type Index struct {
	eng *xorm.Engine // kept alive: xorm closes the pool when the engine is garbage collected
	db  *sql.DB
}

// NewIndex returns an Index over eng.
func NewIndex(eng *xorm.Engine) *Index { return &Index{eng: eng, db: eng.DB().DB} }

// scanQuery is one query, so a not-ready row inserted mid-scan can't be skipped.
const scanQuery = "SELECT id, (total_orders = 0 OR done = 1) AS ok, " +
	"TIMESTAMPDIFF(HOUR, created_at, NOW()) AS age_h " +
	"FROM parsing_documents WHERE id >= ? ORDER BY id"

// Scan streams documents with id >= from in id order. It returns the ready
// ids before the first not-ready document (only if collect) and that
// document as the blocker, or a nil blocker if every row is ready.
// A NULL readiness value counts as not ready.
func (x *Index) Scan(ctx context.Context, from uint64, collect bool) (ids []uint64, blocker *Blocker, err error) {
	rows, err := x.db.QueryContext(ctx, scanQuery, from)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  uint64
			ok  sql.NullBool
			age sql.NullInt64
		)
		if err := rows.Scan(&id, &ok, &age); err != nil {
			return nil, nil, err
		}
		if !ok.Bool {
			return ids, &Blocker{ID: id, AgeHours: age.Int64}, nil
		}
		if collect {
			ids = append(ids, id)
		}
	}
	return ids, nil, rows.Err()
}

// ErrNoSuchTable reports a missing document table.
var ErrNoSuchTable = errors.New("no such document table")

// Docs reads document_<id> tables.
type Docs struct {
	eng *xorm.Engine // kept alive: xorm closes the pool when the engine is garbage collected
	db  *sql.DB
}

// NewDocs returns a Docs over eng.
func NewDocs(eng *xorm.Engine) *Docs { return &Docs{eng: eng, db: eng.DB().DB} }

// TableName returns the table name of document id.
func TableName(id uint64) string { return "document_" + strconv.FormatUint(id, 10) }

// ShowCreate returns the table's CREATE TABLE statement.
func (d *Docs) ShowCreate(ctx context.Context, id uint64) (string, error) {
	var name, ddl string
	err := d.db.QueryRowContext(ctx, "SHOW CREATE TABLE `"+TableName(id)+"`").Scan(&name, &ddl)
	if err != nil {
		return "", tableErr(err, id)
	}
	return ddl, nil
}

// RowFunc receives one row. row is reused; fn must not retain it.
type RowFunc func(cols []*sql.ColumnType, row []sql.RawBytes) error

// StreamRows calls fn for every row of the table. The query has no
// arguments, so it uses the text protocol and values keep the server's
// exact text form.
func (d *Docs) StreamRows(ctx context.Context, id uint64, fn RowFunc) error {
	q, err := d.selectQuery(ctx, id)
	if err != nil {
		return err
	}
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return tableErr(err, id)
	}
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if err := fn(cols, vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// selectQuery reads the table's columns and builds its SELECT.
func (d *Docs) selectQuery(ctx context.Context, id uint64) (string, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT * FROM `"+TableName(id)+"` LIMIT 0")
	if err != nil {
		return "", tableErr(err, id)
	}
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		return "", err
	}
	names := make([]string, len(cols))
	types := make([]string, len(cols))
	for i, c := range cols {
		names[i], types[i] = c.Name(), c.DatabaseTypeName()
	}
	return selectSQL(TableName(id), names, types), nil
}

// selectSQL returns SELECT * unless the table has FLOAT columns. MariaDB
// prints FLOAT with only 6 significant digits, which loses bits, so those
// columns are widened to DOUBLE (exact) whose text form round-trips.
func selectSQL(table string, names, types []string) string {
	if !slices.Contains(types, "FLOAT") {
		return "SELECT * FROM " + quoteIdent(table)
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteIdent(name))
		if types[i] == "FLOAT" {
			b.WriteString("+0e0 AS ")
			b.WriteString(quoteIdent(name))
		}
	}
	b.WriteString(" FROM ")
	b.WriteString(quoteIdent(table))
	return b.String()
}

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// tableErr maps MySQL error 1146 (table doesn't exist) to ErrNoSuchTable.
func tableErr(err error, id uint64) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1146 {
		return fmt.Errorf("%w: %s", ErrNoSuchTable, TableName(id))
	}
	return err
}

// IsTransient reports whether err is worth retrying: lost connections,
// timeouts, deadlocks and server overload.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, ErrNoSuchTable) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1040, 1053, 1205, 1213, 2006, 2013:
			return true
		}
	}
	return false
}
