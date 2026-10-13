//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"

	"autodump/internal/source"
)

// xorm closes an engine's pool from a finalizer, so Index/Docs must keep the
// engine reachable. Regression: "sql: database is closed" after a GC.
func TestEngineSurvivesGC(t *testing.T) {
	host := os.Getenv("IT_DB_HOST")
	if host == "" {
		host = "127.0.0.1:3310"
	}
	pass := os.Getenv("IT_DB_PASSWORD")
	if pass == "" {
		pass = "autodump"
	}
	eng, err := source.Open(source.ConnParams{Login: "root", Password: pass, Host: host, Database: "mysql"})
	if err != nil {
		t.Fatal(err)
	}
	docs := source.NewDocs(eng)
	idx := source.NewIndex(eng)
	eng = nil
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	if _, err := docs.ShowCreate(context.Background(), 987654321); !errors.Is(err, source.ErrNoSuchTable) {
		t.Fatalf("docs after GC: %v", err)
	}
	if _, _, err := idx.Scan(context.Background(), 0, false); err == nil || err.Error() == "sql: database is closed" {
		t.Fatalf("index after GC: %v", err)
	}
	runtime.KeepAlive(idx)
}
