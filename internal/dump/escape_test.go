package dump

import (
	"database/sql"
	"testing"
)

func TestAppendValue(t *testing.T) {
	tests := []struct {
		name, typ string
		in        sql.RawBytes
		want      string
	}{
		{"null", "VARCHAR", nil, "NULL"},
		{"null int", "INT", nil, "NULL"},
		{"null blob", "BLOB", nil, "NULL"},
		{"empty string", "VARCHAR", sql.RawBytes{}, "''"},
		{"plain", "VARCHAR", sql.RawBytes("abc"), "'abc'"},
		{"nul byte", "TEXT", sql.RawBytes("a\x00b"), `'a\0b'`},
		{"quotes", "VARCHAR", sql.RawBytes(`it's "x"`), `'it\'s \"x\"'`},
		{"backslash", "VARCHAR", sql.RawBytes(`a\b\\`), `'a\\b\\\\'`},
		{"newline cr", "TEXT", sql.RawBytes("a\nb\r\n"), `'a\nb\r\n'`},
		{"ctrl-z", "TEXT", sql.RawBytes("a\x1ab"), `'a\Zb'`},
		{"utf8", "VARCHAR", sql.RawBytes("Привет 👋 ёЁ"), "'Привет 👋 ёЁ'"},
		{"datetime", "DATETIME", sql.RawBytes("2024-01-02 03:04:05"), "'2024-01-02 03:04:05'"},
		{"blob hex", "BLOB", sql.RawBytes{0x00, 0x1a, '\'', '\\', 0xff}, "X'001a275cff'"},
		{"empty blob", "LONGBLOB", sql.RawBytes{}, "''"},
		{"varbinary", "VARBINARY", sql.RawBytes("AB"), "X'4142'"},
		{"bit", "BIT", sql.RawBytes{0x05}, "X'05'"},
		{"decimal", "DECIMAL", sql.RawBytes("-0.10"), "-0.10"},
		{"unsigned max", "UNSIGNED BIGINT", sql.RawBytes("18446744073709551615"), "18446744073709551615"},
		{"int", "INT", sql.RawBytes("-42"), "-42"},
		{"tinyint", "UNSIGNED TINYINT", sql.RawBytes("1"), "1"},
		{"double", "DOUBLE", sql.RawBytes("1e-7"), "1e-7"},
		{"float", "FLOAT", sql.RawBytes("0.1"), "0.1"},
		{"year", "YEAR", sql.RawBytes("2024"), "2024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(AppendValue([]byte("prefix:"), tt.typ, tt.in))
			if got != "prefix:"+tt.want {
				t.Fatalf("AppendValue(%q, %q) = %q, want %q", tt.typ, tt.in, got, "prefix:"+tt.want)
			}
		})
	}
}

func BenchmarkAppendQuoted(b *testing.B) {
	v := sql.RawBytes("https://example.com/товар/12345?ref=abc&q=it's fine\n")
	var dst []byte
	b.SetBytes(int64(len(v)))
	for i := 0; i < b.N; i++ {
		dst = AppendValue(dst[:0], "VARCHAR", v)
	}
}
