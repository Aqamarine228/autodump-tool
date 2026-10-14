// Package config loads and validates settings from the environment and .env.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"autodump/internal/schedule"
)

// DBConn holds MySQL connection settings.
type DBConn struct{ Login, Password, Host, Database string }

// Config is the validated application configuration.
type Config struct {
	Database, Documents DBConn
	IndexTable          string // document index table in the DATABASE_* database
	OutputDir           string
	ScheduleTimes       []schedule.HHMM
	RunOnStart          bool
	StartID             uint64
	BatchSize           int
	MergeThreshold      int
	StallHours          int
	TelegramToken       string
	TelegramChatID      string

	TelegramRetryAttempts int
	LogFile               string
	LogFileMB, LogTotalMB int

	Workers, InsertBatchRows, InsertBatchBytes, WriteBufferMB, DBRetryAttempts, DBRetryBaseSec int

	// Phase 2: the third server keeps only the last RetentionDays of documents.
	MirrorEnabled  bool
	Mirror         DBConn // dedicated database; autodump drops document_* tables in it
	MirrorFilesDir string // optional share for the .sql files; "" = tables only
	RetentionDays  int
}

// identRe is a plain, unqualified MariaDB table name. Table names can't be
// bound as query parameters, so only safe names are accepted.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

// loader accumulates problems while reading keys.
type loader struct{ errs []error }

func (l *loader) fail(format string, a ...any) { l.errs = append(l.errs, fmt.Errorf(format, a...)) }

func (l *loader) str(key string) string { return os.Getenv(key) }

func (l *loader) required(key string) string {
	v := os.Getenv(key)
	if v == "" {
		l.fail("%s is required", key)
	}
	return v
}

func (l *loader) strDef(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// int reads a positive integer (or non-negative when zeroOK).
func (l *loader) int(key string, def int, zeroOK bool) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 || (v == 0 && !zeroOK) {
		l.fail("%s must be a positive integer", key)
		return def
	}
	return v
}

func (l *loader) uint(key string) uint64 {
	s := os.Getenv(key)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		l.fail("%s must be an integer >= 0", key)
	}
	return v
}

func (l *loader) boolean(key string) bool {
	s := os.Getenv(key)
	if s == "" {
		return false
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		l.fail("%s must be true or false", key)
	}
	return v
}

func (l *loader) db(prefix, dbKey string) DBConn {
	return DBConn{
		Login:    l.required(prefix + "_SQL_LOGIN"),
		Password: l.str(prefix + "_SQL_PASSWORD"),
		Host:     l.required(prefix + "_SQL_HOST"),
		Database: l.required(dbKey),
	}
}

// Load reads envPath (if it exists; OS env vars take precedence) and returns
// the validated configuration. All problems are reported together.
func Load(envPath string) (Config, error) {
	if envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			if err := godotenv.Load(envPath); err != nil {
				return Config{}, fmt.Errorf("read %s: %w", envPath, err)
			}
		}
	}
	l := &loader{}
	c := Config{
		Database:   l.db("DATABASE", "DATABASE_DATABASE"),
		Documents:  l.db("DOCUMENTS", "DOCUMENTS_DATABASE"),
		IndexTable: l.strDef("DATABASE_TABLE", "parsing_documents"),
		OutputDir:  l.required("OUTPUT_DIR"),

		RunOnStart:     l.boolean("RUN_ON_START"),
		StartID:        l.uint("START_ID"),
		BatchSize:      l.int("BATCH_SIZE", 100, false),
		MergeThreshold: l.int("MERGE_THRESHOLD", 150, false),
		StallHours:     l.int("STALL_HOURS", 24, false),

		TelegramToken:         l.required("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:        l.required("TELEGRAM_CHAT_ID"),
		TelegramRetryAttempts: l.int("TELEGRAM_RETRY_ATTEMPTS", 3, false),

		LogFile:    l.strDef("LOG_FILE", "autodump.log"),
		LogFileMB:  l.int("LOG_FILE_MB", 100, false),
		LogTotalMB: l.int("LOG_TOTAL_MB", 1024, false),

		Workers:          l.int("WORKERS", 2, false),
		InsertBatchRows:  l.int("INSERT_BATCH_ROWS", 1000, false),
		InsertBatchBytes: l.int("INSERT_BATCH_BYTES", 4194304, false),
		WriteBufferMB:    l.int("WRITE_BUFFER_MB", 4, false),
		DBRetryAttempts:  l.int("DB_RETRY_ATTEMPTS", 5, false),
		DBRetryBaseSec:   l.int("DB_RETRY_BASE_SEC", 5, false),
	}

	times, err := schedule.ParseTimes(l.strDef("SCHEDULE_TIMES", "02:00"))
	if err != nil {
		l.fail("SCHEDULE_TIMES: %v", err)
	}
	c.ScheduleTimes = times

	if c.MergeThreshold < c.BatchSize {
		l.fail("MERGE_THRESHOLD (%d) must be >= BATCH_SIZE (%d)", c.MergeThreshold, c.BatchSize)
	}
	if c.LogTotalMB < c.LogFileMB {
		l.fail("LOG_TOTAL_MB (%d) must be >= LOG_FILE_MB (%d)", c.LogTotalMB, c.LogFileMB)
	}
	if !identRe.MatchString(c.IndexTable) {
		l.fail("DATABASE_TABLE %q must be a plain table name (letters, digits, _ or $)", c.IndexTable)
	}
	if c.OutputDir != "" {
		if err := checkWritable(c.OutputDir); err != nil {
			l.fail("OUTPUT_DIR %q is not usable: %v", c.OutputDir, err)
		}
	}

	c.MirrorEnabled = l.boolean("MIRROR_ENABLED")
	if c.MirrorEnabled {
		c.Mirror = l.db("MIRROR", "MIRROR_DATABASE")
		c.MirrorFilesDir = l.str("MIRROR_FILES_DIR")
		c.RetentionDays = l.int("RETENTION_DAYS", 30, false)
		for _, src := range []struct {
			key string
			db  DBConn
		}{{"DATABASE_*", c.Database}, {"DOCUMENTS_*", c.Documents}} {
			if sameDB(c.Mirror, src.db) {
				l.fail("MIRROR_* must point to a dedicated database, not the same one as %s (autodump drops tables there)", src.key)
			}
		}
		if c.MirrorFilesDir != "" {
			if samePath(c.MirrorFilesDir, c.OutputDir) {
				l.fail("MIRROR_FILES_DIR must differ from OUTPUT_DIR (old files are deleted there)")
			} else if err := checkWritable(c.MirrorFilesDir); err != nil {
				l.fail("MIRROR_FILES_DIR %q is not usable: %v", c.MirrorFilesDir, err)
			}
		}
	}

	if len(l.errs) > 0 {
		return Config{}, errors.Join(l.errs...)
	}
	return c, nil
}

// sameDB reports whether a and b name the same database on the same server.
func sameDB(a, b DBConn) bool {
	return a.Database != "" && strings.EqualFold(a.Database, b.Database) && hostKey(a.Host) == hostKey(b.Host)
}

func hostKey(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if !strings.Contains(h, ":") {
		h += ":3306"
	}
	host, port, _ := strings.Cut(h, ":")
	if host == "localhost" || host == "" {
		host = "127.0.0.1"
	}
	return host + ":" + port
}

// samePath compares paths case-insensitively (Windows).
func samePath(a, b string) bool {
	x, err1 := filepath.Abs(a)
	y, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(x), filepath.Clean(y))
}

// checkWritable creates dir if needed and proves it is writable with a probe file.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(filepath.Clean(name))
}
