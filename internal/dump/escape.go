package dump

import (
	"database/sql"
	"encoding/hex"
	"strings"
)

// kind is how a column's text value is rendered in SQL.
type kind uint8

const (
	kindString kind = iota // single-quoted with backslash escapes
	kindNumber             // raw server text
	kindBinary             // X'hex'
)

// classify maps a driver DatabaseTypeName (e.g. "UNSIGNED BIGINT") to a kind.
func classify(dbType string) kind {
	switch strings.TrimPrefix(dbType, "UNSIGNED ") {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT",
		"FLOAT", "DOUBLE", "DECIMAL", "YEAR":
		return kindNumber
	case "BIT", "BINARY", "VARBINARY", "TINYBLOB", "BLOB", "MEDIUMBLOB", "LONGBLOB", "GEOMETRY":
		return kindBinary
	}
	return kindString
}

// escapes maps a byte to the letter that follows the backslash, or 0 if safe.
var escapes = [256]byte{
	0x00: '0', '\n': 'n', '\r': 'r', '\\': '\\', '\'': '\'', '"': '"', 0x1a: 'Z',
}

// AppendValue appends v as a SQL literal for a column of type dbType.
func AppendValue(dst []byte, dbType string, v sql.RawBytes) []byte {
	return appendKind(dst, classify(dbType), v)
}

func appendKind(dst []byte, k kind, v []byte) []byte {
	if v == nil {
		return append(dst, "NULL"...)
	}
	switch k {
	case kindNumber:
		return append(dst, v...)
	case kindBinary:
		if len(v) == 0 {
			return append(dst, "''"...)
		}
		dst = append(dst, "X'"...)
		dst = hex.AppendEncode(dst, v)
		return append(dst, '\'')
	}
	return appendQuoted(dst, v)
}

// appendQuoted copies runs of safe bytes and escapes the rest.
func appendQuoted(dst, v []byte) []byte {
	dst = append(dst, '\'')
	start := 0
	for i, c := range v {
		if e := escapes[c]; e != 0 {
			dst = append(dst, v[start:i]...)
			dst = append(dst, '\\', e)
			start = i + 1
		}
	}
	dst = append(dst, v[start:]...)
	return append(dst, '\'')
}

// appendIdent appends a backtick-quoted identifier.
func appendIdent(dst []byte, name string) []byte {
	dst = append(dst, '`')
	for i := 0; i < len(name); i++ {
		if name[i] == '`' {
			dst = append(dst, '`')
		}
		dst = append(dst, name[i])
	}
	return append(dst, '`')
}
