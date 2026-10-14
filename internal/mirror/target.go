package mirror

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"xorm.io/xorm"

	"autodump/internal/dump"
	"autodump/internal/source"
)

const stagingSuffix = "__part"

// StagingName is the table a document is imported into before it is renamed
// to its real name, so a half-imported table never looks complete.
func StagingName(id uint64) string { return source.TableName(id) + stagingSuffix }

var tableRe = regexp.MustCompile(`^document_(\d+)(` + stagingSuffix + `)?$`)

// DBTarget is the mirror database.
type DBTarget struct {
	eng *xorm.Engine // kept alive: xorm closes the pool when the engine is garbage collected
	db  *sql.DB
}

// NewDBTarget returns a Target over eng's database.
func NewDBTarget(eng *xorm.Engine) *DBTarget { return &DBTarget{eng: eng, db: eng.DB().DB} }

// Tables lists document_<id> and leftover staging tables of the database.
func (t *DBTarget) Tables(ctx context.Context) (done, staging []uint64, err error) {
	rows, err := t.db.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, err
		}
		m := tableRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		id, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			continue
		}
		if m[2] != "" {
			staging = append(staging, id)
		} else {
			done = append(done, id)
		}
	}
	slices.Sort(done)
	slices.Sort(staging)
	return done, staging, rows.Err()
}

// Drop drops a document or staging table; table must be such a name.
func (t *DBTarget) Drop(ctx context.Context, table string) error {
	if !tableRe.MatchString(table) {
		return fmt.Errorf("refusing to drop %q: not a document table", table)
	}
	_, err := t.db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`")
	return err
}

// Import copies one document into the mirror: it fills a staging table on a
// single connection (session settings are per connection), then replaces
// document_<id> with it.
func (t *DBTarget) Import(ctx context.Context, id uint64, ddl string, stream func(source.RowFunc) error, opts dump.Options) (rows int64, err error) {
	name, staging := source.TableName(id), StagingName(id)
	prefix := "CREATE TABLE `" + name + "`"
	if !strings.HasPrefix(ddl, prefix) {
		return 0, fmt.Errorf("unexpected DDL for %s: %.60q", name, ddl)
	}
	ddl = "CREATE TABLE `" + staging + "`" + ddl[len(prefix):]

	conn, err := t.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	for _, s := range dump.SessionSQL {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			return 0, err
		}
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS `"+staging+"`")
		}
	}()

	w := &execWriter{ctx: ctx, conn: conn}
	if rows, err = dump.WriteTable(w, staging, ddl, stream, opts); err != nil {
		return rows, err
	}
	if w.buf.Len() > 0 {
		return rows, fmt.Errorf("incomplete statement left after %s", name)
	}
	if _, err = conn.ExecContext(ctx, "DROP TABLE IF EXISTS `"+name+"`"); err != nil {
		return rows, err
	}
	_, err = conn.ExecContext(ctx, "RENAME TABLE `"+staging+"` TO `"+name+"`")
	return rows, err
}

// execWriter executes what dump writes, one statement at a time. dump ends
// every statement with ";\n" at the end of a Write call (see WriteTable).
type execWriter struct {
	ctx  context.Context
	conn *sql.Conn
	buf  bytes.Buffer
}

func (w *execWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if !bytes.HasSuffix(w.buf.Bytes(), []byte(";\n")) {
		return len(p), nil
	}
	stmt := w.buf.Bytes()
	_, err := w.conn.ExecContext(w.ctx, string(stmt[:len(stmt)-2]))
	w.buf.Reset()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
