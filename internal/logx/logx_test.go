package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaxBackups(t *testing.T) {
	for _, tc := range []struct{ file, total, want int }{
		{100, 1024, 9}, {100, 100, 0}, {100, 50, 0}, {10, 100, 9},
	} {
		if got := maxBackups(tc.file, tc.total); got != tc.want {
			t.Errorf("maxBackups(%d,%d)=%d want %d", tc.file, tc.total, got, tc.want)
		}
	}
}

func TestRedact(t *testing.T) {
	got := Redact("token abc and pass xyz, abc", "abc", "", "xyz")
	if got != "token *** and pass ***, ***" {
		t.Fatal(got)
	}
	if Redact("same", "") != "same" {
		t.Fatal("empty secret must be ignored")
	}
}

func TestNewWritesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	l := New(p, 1, 10, false)
	l.Println("hello logx")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "hello logx") {
		t.Fatalf("file: %q", b)
	}
}
