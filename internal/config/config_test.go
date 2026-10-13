package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodump/internal/schedule"
)

var allKeys = []string{
	"DATABASE_SQL_LOGIN", "DATABASE_SQL_PASSWORD", "DATABASE_SQL_HOST", "DATABASE_DATABASE",
	"DOCUMENTS_SQL_LOGIN", "DOCUMENTS_SQL_PASSWORD", "DOCUMENTS_SQL_HOST", "DOCUMENTS_DATABASE",
	"OUTPUT_DIR", "SCHEDULE_TIMES", "RUN_ON_START", "START_ID", "BATCH_SIZE", "MERGE_THRESHOLD",
	"STALL_HOURS", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "TELEGRAM_RETRY_ATTEMPTS",
	"LOG_FILE", "LOG_FILE_MB", "LOG_TOTAL_MB", "WORKERS", "INSERT_BATCH_ROWS",
	"INSERT_BATCH_BYTES", "WRITE_BUFFER_MB", "DB_RETRY_ATTEMPTS", "DB_RETRY_BASE_SEC",
}

// clean blanks every known key for the test (restored automatically).
func clean(t *testing.T) {
	t.Helper()
	for _, k := range allKeys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func minimal(t *testing.T) string {
	t.Helper()
	clean(t)
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("DATABASE_SQL_LOGIN", "u1")
	t.Setenv("DATABASE_SQL_PASSWORD", "dbpass-secret")
	t.Setenv("DATABASE_SQL_HOST", "h1:3306")
	t.Setenv("DATABASE_DATABASE", "d1")
	t.Setenv("DOCUMENTS_SQL_LOGIN", "u2")
	t.Setenv("DOCUMENTS_SQL_HOST", "h2:3306")
	t.Setenv("DOCUMENTS_DATABASE", "d2")
	t.Setenv("OUTPUT_DIR", out)
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok-secret")
	t.Setenv("TELEGRAM_CHAT_ID", "42")
	return out
}

func TestDefaults(t *testing.T) {
	out := minimal(t)
	c, err := Load(filepath.Join(t.TempDir(), "missing.env"))
	if err != nil {
		t.Fatal(err)
	}
	if c.OutputDir != out || c.Database.Login != "u1" || c.Documents.Database != "d2" || c.Documents.Password != "" {
		t.Fatalf("%+v", c)
	}
	if len(c.ScheduleTimes) != 1 || c.ScheduleTimes[0] != (schedule.HHMM{H: 2}) {
		t.Fatalf("schedule %v", c.ScheduleTimes)
	}
	if c.RunOnStart || c.StartID != 0 || c.BatchSize != 100 || c.MergeThreshold != 150 || c.StallHours != 24 ||
		c.TelegramRetryAttempts != 3 || c.LogFile != "autodump.log" || c.LogFileMB != 100 || c.LogTotalMB != 1024 ||
		c.Workers != 2 || c.InsertBatchRows != 1000 || c.InsertBatchBytes != 4194304 || c.WriteBufferMB != 4 ||
		c.DBRetryAttempts != 5 || c.DBRetryBaseSec != 5 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestMissingRequired(t *testing.T) {
	clean(t)
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, k := range []string{"DATABASE_SQL_LOGIN", "DATABASE_SQL_HOST", "DATABASE_DATABASE", "DOCUMENTS_SQL_LOGIN",
		"DOCUMENTS_SQL_HOST", "DOCUMENTS_DATABASE", "OUTPUT_DIR", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error should mention %s: %v", k, err)
		}
	}
	if strings.Contains(err.Error(), "DATABASE_SQL_PASSWORD") {
		t.Error("password is optional")
	}
}

func TestBadValuesCollected(t *testing.T) {
	minimal(t)
	t.Setenv("SCHEDULE_TIMES", "02:00,9:5")
	t.Setenv("BATCH_SIZE", "abc")
	t.Setenv("WORKERS", "0")
	t.Setenv("START_ID", "-1")
	t.Setenv("RUN_ON_START", "maybe")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, k := range []string{"SCHEDULE_TIMES", "BATCH_SIZE", "WORKERS", "START_ID", "RUN_ON_START"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("missing %s in: %v", k, err)
		}
	}
}

func TestMergeLessThanBatch(t *testing.T) {
	minimal(t)
	t.Setenv("BATCH_SIZE", "200")
	t.Setenv("MERGE_THRESHOLD", "150")
	t.Setenv("LOG_FILE_MB", "500")
	t.Setenv("LOG_TOTAL_MB", "100")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "MERGE_THRESHOLD") || !strings.Contains(err.Error(), "LOG_TOTAL_MB") {
		t.Fatalf("%v", err)
	}
}

func TestSecretsNotInErrors(t *testing.T) {
	minimal(t)
	t.Setenv("WORKERS", "x")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "dbpass-secret") || strings.Contains(err.Error(), "tok-secret") {
		t.Fatal("secret leaked")
	}
}

func TestOSEnvOverridesFile(t *testing.T) {
	minimal(t)
	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("BATCH_SIZE=10\nWORKERS=7\nMERGE_THRESHOLD=20\n"), 0o600)
	t.Setenv("WORKERS", "3")
	// BATCH_SIZE / MERGE_THRESHOLD come from the file (godotenv sets them; clean afterwards).
	t.Cleanup(func() { os.Unsetenv("BATCH_SIZE"); os.Unsetenv("MERGE_THRESHOLD") })
	c, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 3 {
		t.Fatalf("OS env must win, got %d", c.Workers)
	}
	if c.BatchSize != 10 || c.MergeThreshold != 20 {
		t.Fatalf("file values not loaded: %+v", c)
	}
}

func TestOutputDirUnwritable(t *testing.T) {
	minimal(t)
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o600)
	t.Setenv("OUTPUT_DIR", filepath.Join(f, "sub")) // parent is a file
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "OUTPUT_DIR") {
		t.Fatalf("%v", err)
	}
}
