//go:build integration

// Integration tests against a real MariaDB. Defaults match the local Docker
// container; override with IT_DB_HOST, IT_DB_PASSWORD, IT_DB_CONTAINER.
package integration

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"autodump/internal/dump"
	"autodump/internal/source"
	"xorm.io/xorm"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	dbHost      = env("IT_DB_HOST", "127.0.0.1:3310")
	dbPassword  = env("IT_DB_PASSWORD", "autodump")
	dbContainer = env("IT_DB_CONTAINER", "autodump-mariadb")
)

func params(db string) source.ConnParams {
	return source.ConnParams{Login: "root", Password: dbPassword, Host: dbHost, Database: db}
}

// freshDB creates an empty database, dropped when the test ends.
func freshDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	admin, err := sql.Open("mysql", source.DSN(params("")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	mustExec(t, admin, "DROP DATABASE IF EXISTS `"+name+"`")
	mustExec(t, admin, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS `" + name + "`") })

	db, err := sql.Open("mysql", source.DSN(params(name)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func openEngine(t *testing.T, name string) *xorm.Engine {
	t.Helper()
	eng, err := source.Open(params(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng
}

const parsingDocsDDL = "CREATE TABLE parsing_documents (" +
	"id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, agent_id BIGINT UNSIGNED, " +
	"done TINYINT(1) NOT NULL DEFAULT 0, total_orders INT NOT NULL DEFAULT 0, " +
	"finished_orders INT NOT NULL DEFAULT 0, created_at DATETIME NULL, updated_at DATETIME NULL)"

// resultDDL models prctrl's Result struct, plus a TIMESTAMP column.
func resultDDL(table string) string {
	return "CREATE TABLE `" + table + "` (" +
		"id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, created_at DATETIME NULL, updated_at DATETIME NULL, " +
		"comparison_id BIGINT UNSIGNED NOT NULL DEFAULT 0, product_id BIGINT UNSIGNED NOT NULL DEFAULT 0, " +
		"domen_id BIGINT UNSIGNED NOT NULL DEFAULT 0, " +
		"rrc DOUBLE NULL, price DOUBLE NULL, old_price DECIMAL(12,2) NULL, cashback DECIMAL(12,2) NULL, promo FLOAT NULL, " +
		"need_confirmed TINYINT(1) NOT NULL DEFAULT 0, demping TINYINT(1) NOT NULL DEFAULT 0, " +
		"status VARCHAR(32) NOT NULL DEFAULT '', availability VARCHAR(255) NULL, inner_code VARCHAR(255) NULL, " +
		"delivery VARCHAR(255) NULL, warranty VARCHAR(255) NULL, additional_field VARCHAR(255) NULL, " +
		"screenshot TEXT NULL, is_available TINYINT(1) NOT NULL DEFAULT 0, seen_at TIMESTAMP NULL DEFAULT NULL, " +
		"PRIMARY KEY (id), KEY idx_product (product_id)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='результаты'"
}

const resultCols = "(id, created_at, updated_at, comparison_id, product_id, domen_id, rrc, price, old_price, " +
	"cashback, promo, need_confirmed, demping, status, availability, inner_code, delivery, warranty, " +
	"additional_field, screenshot, is_available, seen_at)"

// insertResults inserts rows of 22 values with multi-row prepared INSERTs.
func insertResults(t *testing.T, db *sql.DB, table string, rows [][]any) {
	t.Helper()
	const per = 400
	for len(rows) > 0 {
		n := min(per, len(rows))
		ph := strings.TrimSuffix(strings.Repeat("(?"+strings.Repeat(",?", 21)+"),", n), ",")
		var args []any
		for _, r := range rows[:n] {
			args = append(args, r...)
		}
		mustExec(t, db, "INSERT INTO `"+table+"` "+resultCols+" VALUES "+ph, args...)
		rows = rows[n:]
	}
}

// randDouble returns a finite double with a random bit pattern (any exponent).
func randDouble(r *rand.Rand) float64 {
	for {
		f := math.Float64frombits(r.Uint64())
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
}

func randFloat(r *rand.Rand) float32 {
	for {
		f := math.Float32frombits(r.Uint32())
		if !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) {
			return f
		}
	}
}

func buildFixture(t *testing.T, db *sql.DB) []uint64 {
	t.Helper()
	mustExec(t, db, parsingDocsDDL)
	mustExec(t, db, "INSERT INTO parsing_documents (id, done, total_orders, created_at) VALUES "+
		"(1,1,5,NOW()),(2,1,5,NOW()),(3,0,0,NOW()),(4,1,9,NOW()),(5,0,7,NOW())")

	// document_1: hand-picked edge values.
	mustExec(t, db, resultDDL("document_1"))
	maxU := uint64(math.MaxUint64)
	insertResults(t, db, "document_1", [][]any{
		{1, "2024-01-02 03:04:05", nil, maxU, 1, 2, 0.1, 1e-7, "-0.10", "123456.79", float32(0.1),
			1, 0, "ok", "в наличии", "A'B\"C\\D", "line1\nline2\r\n", nil, "😀 emoji ñ", "x\x00y\x1az", 1, "2024-03-31 01:30:00"},
		{2, "0000-00-00 00:00:00", "2038-01-19 03:14:08", 0, 0, 0, 123456.789, -0.0, "-9999999999.99", "0.00", float32(-1e-7),
			0, 1, "", "", "", "", "", "", "", 0, "1970-01-01 00:00:01"},
		{3, nil, nil, 1, 1, 1, nil, nil, nil, nil, nil,
			0, 0, "null-row", nil, nil, nil, nil, nil, nil, 0, nil},
		{4, "2024-12-31 23:59:59", nil, 7, 8, 9, math.MaxFloat64, math.SmallestNonzeroFloat64, "0.01", "-0.01", float32(math.MaxFloat32),
			1, 1, "Ёжик", strings.Repeat("я", 255), nil, nil, nil, nil, strings.Repeat("Длинный текст ", 500), 1, "2030-06-15 12:00:00"},
	})

	// document_2: binary-ish columns with random bytes, including empty values.
	mustExec(t, db, "CREATE TABLE document_2 (id INT UNSIGNED NOT NULL PRIMARY KEY, data BLOB NULL, "+
		"bits BIT(8) NULL, vb VARBINARY(32) NULL, bin BINARY(4) NULL, yr YEAR NULL, d DATE NULL, tm TIME NULL, "+
		"en ENUM('a','b''c') NULL, js LONGTEXT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	r := rand.New(rand.NewSource(1))
	special := []byte{0x00, 0x1a, '\'', '"', '\\', '\n', '\r', 0xff, 0x80}
	for i := 1; i <= 50; i++ {
		data := make([]byte, r.Intn(300))
		r.Read(data)
		data = append(data, special...)
		vb := make([]byte, r.Intn(32))
		r.Read(vb)
		mustExec(t, db, "INSERT INTO document_2 VALUES (?,?,?,?,?,?,?,?,?,?)",
			i, data, []byte{byte(i * 5)}, vb, []byte{0, 1, 0x27, 0x5c}, 1901+i, "2024-02-29", "-838:59:59", "b'c", `{"k":"v\n"}`)
	}
	mustExec(t, db, "INSERT INTO document_2 (id, data, vb) VALUES (51, '', '')")
	mustExec(t, db, "INSERT INTO document_2 (id) VALUES (52)")

	// document_3: empty.
	mustExec(t, db, resultDDL("document_3"))

	// document_4: 20k rows of random values to exercise splitting and float text round-trip.
	mustExec(t, db, resultDDL("document_4"))
	rows := make([][]any, 20000)
	for i := range rows {
		var rrc, promo any = randDouble(r), randFloat(r)
		if i%7 == 0 {
			rrc, promo = nil, nil
		}
		rows[i] = []any{i + 1, "2024-05-01 10:00:00", "2024-05-02 11:00:00", r.Uint64(), r.Int63(), r.Uint32(),
			rrc, r.NormFloat64() * math.Pow(10, float64(r.Intn(12))), "12.34", nil, promo,
			r.Intn(2), r.Intn(2), "done", "есть", "C-1", "1-2 дня", "12 мес", "доп'поле", "https://x/" + string(rune('a'+i%26)), 1, "2024-05-01 10:00:00"}
	}
	insertResults(t, db, "document_4", rows)

	return []uint64{1, 2, 3, 4}
}

// exportAll writes one dump file for ids and returns its size.
func exportAll(t *testing.T, eng *xorm.Engine, ids []uint64, path string, opts dump.Options) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	docs := source.NewDocs(eng)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 4<<20)
	if err := dump.WriteFileHeader(w); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, id := range ids {
		ddl, err := docs.ShowCreate(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		n, err := dump.WriteTable(w, source.TableName(id), ddl, func(fn source.RowFunc) error {
			return docs.StreamRows(ctx, id, fn)
		}, opts)
		if err != nil {
			t.Fatalf("document_%d: %v", id, err)
		}
		total += n
	}
	if err := dump.WriteFileFooter(w, len(ids), total); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	return total, st.Size()
}

// restore loads a dump file with the mysql CLI inside the container.
func restore(t *testing.T, path, db string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", dbContainer, "mysql", "-uroot", "-p"+dbPassword, db)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
}

func checksum(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var name string
	var sum sql.NullInt64
	if err := db.QueryRow("CHECKSUM TABLE `"+table+"`").Scan(&name, &sum); err != nil {
		t.Fatal(err)
	}
	return sum.Int64
}

func showCreate(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var name, ddl string
	if err := db.QueryRow("SHOW CREATE TABLE `"+table+"`").Scan(&name, &ddl); err != nil {
		t.Fatal(err)
	}
	return ddl
}

func TestRoundTrip(t *testing.T) {
	src := freshDB(t, "autodump_it_src")
	dst := freshDB(t, "autodump_it_dst")
	buildFixture(t, src)
	eng := openEngine(t, "autodump_it_src")

	ids, blocker, err := source.NewIndex(eng, "parsing_documents").Scan(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []uint64{1, 2, 3, 4}) || blocker == nil || blocker.ID != 5 {
		t.Fatalf("scan: ids %v blocker %+v", ids, blocker)
	}

	path := filepath.Join(t.TempDir(), "1-4.sql")
	rows, size := exportAll(t, eng, ids, path, dump.Options{InsertBatchRows: 1000, InsertBatchBytes: 256 << 10})
	if want := int64(4 + 52 + 0 + 20000); rows != want {
		t.Fatalf("rows = %d, want %d", rows, want)
	}
	body, _ := os.ReadFile(path)
	if c := strings.Count(string(body), "INSERT INTO `document_4`"); c < 20 {
		t.Fatalf("document_4 split into %d INSERTs, want >= 20", c)
	}
	if !strings.HasSuffix(string(body), "-- autodump complete: 4 tables, 20056 rows\n") {
		t.Fatal("missing completion footer")
	}
	t.Logf("dump size %d bytes", size)

	restore(t, path, "autodump_it_dst")
	for _, id := range ids {
		tbl := source.TableName(id)
		if a, b := showCreate(t, src, tbl), showCreate(t, dst, tbl); a != b {
			t.Errorf("%s DDL differs:\nsrc: %s\ndst: %s", tbl, a, b)
		}
		if a, b := checksum(t, src, tbl), checksum(t, dst, tbl); a != b {
			t.Errorf("%s checksum differs: src %d dst %d", tbl, a, b)
			diffColumns(t, src, tbl)
		}
	}
}

func TestIndexScan(t *testing.T) {
	db := freshDB(t, "autodump_it_idx")
	mustExec(t, db, parsingDocsDDL)
	x := source.NewIndex(openEngine(t, "autodump_it_idx"), "parsing_documents")
	ctx := context.Background()

	scan := func(from uint64, collect bool) ([]uint64, *source.Blocker) {
		t.Helper()
		ids, b, err := x.Scan(ctx, from, collect)
		if err != nil {
			t.Fatal(err)
		}
		return ids, b
	}

	// Blocker in the middle.
	mustExec(t, db, "INSERT INTO parsing_documents (id, done, total_orders, created_at) VALUES "+
		"(1,1,3,NOW()),(2,1,3,NOW()),(3,1,3,NOW()),(4,1,3,NOW()),(5,1,3,NOW()),(6,0,3,NOW()),(7,1,3,NOW())")
	if ids, b := scan(1, true); !reflect.DeepEqual(ids, []uint64{1, 2, 3, 4, 5}) || b == nil || b.ID != 6 {
		t.Fatalf("blocker case: ids %v blocker %+v", ids, b)
	}
	if ids, b := scan(1, false); ids != nil || b == nil || b.ID != 6 {
		t.Fatalf("no-collect case: ids %v blocker %+v", ids, b)
	}
	if ids, b := scan(7, true); !reflect.DeepEqual(ids, []uint64{7}) || b != nil {
		t.Fatalf("from=7: ids %v blocker %+v", ids, b)
	}

	// Gaps, and total_orders = 0 counts as ready even when not done.
	mustExec(t, db, "TRUNCATE parsing_documents")
	mustExec(t, db, "INSERT INTO parsing_documents (id, done, total_orders) VALUES (10,1,4),(12,1,0),(15,0,0)")
	if ids, b := scan(1, true); !reflect.DeepEqual(ids, []uint64{10, 12, 15}) || b != nil {
		t.Fatalf("gaps: ids %v blocker %+v", ids, b)
	}
	if ids, b := scan(11, true); !reflect.DeepEqual(ids, []uint64{12, 15}) || b != nil {
		t.Fatalf("from=11: ids %v blocker %+v", ids, b)
	}
	if ids, b := scan(16, true); ids != nil || b != nil {
		t.Fatalf("past end: ids %v blocker %+v", ids, b)
	}

	// Blocker age, and NULL created_at.
	mustExec(t, db, "TRUNCATE parsing_documents")
	mustExec(t, db, "INSERT INTO parsing_documents (id, done, total_orders, created_at) VALUES "+
		"(20,1,1,NOW()),(21,0,1,NOW() - INTERVAL 30 HOUR),(22,0,1,NULL)")
	if ids, b := scan(1, true); !reflect.DeepEqual(ids, []uint64{20}) || b == nil || b.ID != 21 || b.AgeHours < 29 {
		t.Fatalf("age: ids %v blocker %+v", ids, b)
	}
	if _, b := scan(22, true); b == nil || b.ID != 22 || b.AgeHours != 0 {
		t.Fatalf("null created_at: blocker %+v", b)
	}
}

func TestMissingTable(t *testing.T) {
	freshDB(t, "autodump_it_miss")
	docs := source.NewDocs(openEngine(t, "autodump_it_miss"))
	ctx := context.Background()
	if _, err := docs.ShowCreate(ctx, 999); !errors.Is(err, source.ErrNoSuchTable) {
		t.Fatalf("ShowCreate: %v", err)
	}
	err := docs.StreamRows(ctx, 999, func([]*sql.ColumnType, []sql.RawBytes) error { return nil })
	if !errors.Is(err, source.ErrNoSuchTable) || source.IsTransient(err) {
		t.Fatalf("StreamRows: %v", err)
	}
}

func TestThroughput(t *testing.T) {
	db := freshDB(t, "autodump_it_big")
	mustExec(t, db, resultDDL("document_100"))
	insertResults(t, db, "document_100", [][]any{{1, "2024-05-01 10:00:00", "2024-05-01 10:00:00", 1, 2, 3,
		99.5, 1234.56, "1000.00", "5.50", float32(0.25), 1, 0, "done", "в наличии", "SKU-1", "1-2 дня", "12 мес",
		"доп. поле 'x'", strings.Repeat("https://example.com/screenshot/товар ", 20), 1, "2024-05-01 10:00:00"}})
	// Double the table until it holds about 50 MB of row data.
	cols := strings.TrimPrefix(strings.Trim(resultCols, "()"), "id, ")
	for i := 0; i < 16; i++ {
		mustExec(t, db, "INSERT INTO document_100 ("+cols+") SELECT "+cols+" FROM document_100")
	}

	eng := openEngine(t, "autodump_it_big")
	path := filepath.Join(t.TempDir(), "100-100.sql")
	start := time.Now()
	rows, size := exportAll(t, eng, []uint64{100}, path, dump.Options{InsertBatchRows: 1000, InsertBatchBytes: 4 << 20})
	el := time.Since(start)
	t.Logf("exported %d rows, %.1f MB in %v: %.1f MB/s", rows, float64(size)/1e6, el.Round(time.Millisecond),
		float64(size)/1e6/el.Seconds())
}

// diffColumns logs, per column, rows whose values differ between src and dst.
func diffColumns(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	rs, err := db.Query("SELECT column_name FROM information_schema.columns WHERE table_schema = 'autodump_it_src' AND table_name = ?", table)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rs.Next() {
		var c string
		rs.Scan(&c)
		cols = append(cols, c)
	}
	rs.Close()
	for _, c := range cols {
		q := "SELECT s.id, CAST(s.`" + c + "` AS CHAR), CAST(d.`" + c + "` AS CHAR), HEX(s.`" + c + "`), HEX(d.`" + c + "`) " +
			"FROM autodump_it_src.`" + table + "` s JOIN autodump_it_dst.`" + table + "` d USING (id) " +
			"WHERE NOT (s.`" + c + "` <=> d.`" + c + "`) LIMIT 3"
		rs, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var id int64
			var a, b, ha, hb sql.NullString
			rs.Scan(&id, &a, &b, &ha, &hb)
			t.Logf("  %s.%s id=%d src=%s dst=%s (hex %s / %s)", table, c, id, a.String, b.String, ha.String, hb.String)
		}
		rs.Close()
	}
}
