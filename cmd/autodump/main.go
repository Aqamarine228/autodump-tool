// Command autodump exports finished prctrl documents into SQL files on a
// schedule and reports problems to Telegram.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"autodump/internal/alert"
	"autodump/internal/config"
	"autodump/internal/dump"
	"autodump/internal/export"
	"autodump/internal/logx"
	"autodump/internal/output"
	"autodump/internal/schedule"
	"autodump/internal/source"
)

func main() {
	envPath := flag.String("env", defaultEnvPath(), "path to the .env file")
	once := flag.Bool("once", false, "run one export now and exit")
	bench := flag.Int("bench", 0, "export at most N documents now, print throughput and exit")
	outDir := flag.String("out", "", "override OUTPUT_DIR (useful with -bench)")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit")
	flag.Parse()

	if *outDir != "" {
		os.Setenv("OUTPUT_DIR", *outDir)
	}
	cfg, err := config.Load(*envPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error (%s):\n%v\n", *envPath, err)
		alertConfigError(err)
		os.Exit(2)
	}
	if *checkConfig {
		fmt.Println("configuration OK")
		return
	}

	logger := logx.New(cfg.LogFile, cfg.LogFileMB, cfg.LogTotalMB, true)
	host, _ := os.Hostname()
	var sender alert.Sender = alert.NewTelegram(cfg.TelegramToken, cfg.TelegramChatID, cfg.TelegramRetryAttempts)
	if *bench > 0 {
		sender = logOnlySender{} // benchmarks must not page anyone
	}
	a := &app{cfg: cfg, log: logger, notify: alert.New(sender, logger, host)}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case *bench > 0:
		os.Exit(a.runNow(ctx, *bench))
	case *once:
		os.Exit(a.runNow(ctx, 0))
	default:
		a.serve(ctx)
	}
}

type app struct {
	cfg    config.Config
	log    *log.Logger
	notify *alert.Alerter

	mu      sync.Mutex // guards runner
	runner  *export.Runner
	running sync.Mutex // held while an export runs
	wg      sync.WaitGroup
}

// serve runs forever, triggering exports at the scheduled times.
func (a *app) serve(ctx context.Context) {
	times := a.cfg.ScheduleTimes
	next := schedule.Next(time.Now(), times)
	a.notify.Info(fmt.Sprintf("started: schedule %s, output %s, next run %s",
		formatTimes(times), a.cfg.OutputDir, next.Format("2006-01-02 15:04")))

	if a.cfg.RunOnStart {
		a.trigger(ctx)
	}
	// A short ticker (instead of one long timer) copes with sleep/hibernate and clock changes.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Printf("shutting down, waiting for the running export to stop")
			a.wg.Wait()
			a.notify.Info("stopped")
			return
		case now := <-ticker.C:
			if !now.Before(next) {
				a.trigger(ctx)
				next = schedule.Next(now, times)
				a.log.Printf("next run at %s", next.Format("2006-01-02 15:04"))
			}
		}
	}
}

// trigger starts an export, or only a stall check when one is still running.
func (a *app) trigger(ctx context.Context) {
	if !a.running.TryLock() {
		a.log.Printf("previous export still running, skipping this run (stall check only)")
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.guard("stall check", func() error {
				r, err := a.ensureRunner()
				if err != nil {
					return err
				}
				return r.StallCheck(ctx)
			})
		}()
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.running.Unlock()
		a.guard("export", func() error { return a.export(ctx, 0) })
	}()
}

// runNow runs a single export in the foreground and returns the exit code.
func (a *app) runNow(ctx context.Context, limit int) int {
	failed := false
	a.guard("export", func() error {
		err := a.export(ctx, limit)
		failed = err != nil
		return err
	})
	if failed {
		return 1
	}
	return 0
}

func (a *app) export(ctx context.Context, limit int) error {
	a.notify.ResetRun()
	r, err := a.ensureRunner()
	if err != nil {
		return err
	}
	r.SetLimit(limit)
	sum, err := r.Run(ctx)
	secs := sum.Duration.Seconds()
	mbps, gbph := 0.0, 0.0
	if secs > 0 {
		mbps = float64(sum.Bytes) / 1e6 / secs
		gbph = float64(sum.Bytes) / 1e9 / (secs / 3600)
	}
	a.log.Printf("run finished: %d document(s), %d file(s), %d row(s), %.1f MB in %s (%.1f MB/s, %.1f GB/h); waiting on: %s",
		sum.Docs, sum.Files, sum.Rows, float64(sum.Bytes)/1e6, sum.Duration.Round(time.Second), mbps, gbph, blockerText(sum.Blocker))
	return err
}

// ensureRunner connects to both databases once; on failure the next run retries.
func (a *app) ensureRunner() (*export.Runner, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runner != nil {
		return a.runner, nil
	}
	idxConn := conn(a.cfg.Database)
	idxConn.ServerTimeZone = true // stall age: created_at vs NOW() in the server's own zone
	idxEng, err := source.Open(idxConn)
	if err != nil {
		return nil, &connectError{"index (DATABASE_*)", err}
	}
	docEng, err := source.Open(conn(a.cfg.Documents))
	if err != nil {
		idxEng.Close()
		return nil, &connectError{"documents (DOCUMENTS_*)", err}
	}
	c := a.cfg
	a.runner = export.New(export.Config{
		StartID:        c.StartID,
		BatchSize:      c.BatchSize,
		MergeThreshold: c.MergeThreshold,
		Workers:        c.Workers,
		StallHours:     c.StallHours,
		WriteBufferMB:  c.WriteBufferMB,
		RetryAttempts:  c.DBRetryAttempts,
		RetryBase:      time.Duration(c.DBRetryBaseSec) * time.Second,
		Dump:           dump.Options{InsertBatchRows: c.InsertBatchRows, InsertBatchBytes: c.InsertBatchBytes},
	}, source.NewIndex(idxEng), source.NewDocs(docEng), output.NewDir(c.OutputDir), a.notify, a.log)
	return a.runner, nil
}

// guard runs fn, turning errors and panics into alerts. It never lets the process die.
func (a *app) guard(what string, fn func() error) {
	defer func() {
		if p := recover(); p != nil {
			a.notify.Failure("panic", fmt.Sprintf("%s crashed: %v\n%s", what, p, debug.Stack()))
		}
	}()
	err := fn()
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	key := export.AlertKey(err)
	var ce *connectError
	if errors.As(err, &ce) {
		key = "db_connect"
	}
	a.notify.Failure(key, fmt.Sprintf("%s failed: %s", what, a.redact(err.Error())))
}

func (a *app) redact(s string) string {
	return logx.Redact(s, a.cfg.Database.Password, a.cfg.Documents.Password, a.cfg.TelegramToken)
}

type connectError struct {
	which string
	err   error
}

func (e *connectError) Error() string {
	return "cannot connect to " + e.which + " database: " + e.err.Error()
}
func (e *connectError) Unwrap() error { return e.err }

func conn(c config.DBConn) source.ConnParams {
	return source.ConnParams{Login: c.Login, Password: c.Password, Host: c.Host, Database: c.Database}
}

func blockerText(b *source.Blocker) string {
	if b == nil {
		return "nothing (all ready documents exported)"
	}
	return fmt.Sprintf("document %d, not ready for %d h", b.ID, b.AgeHours)
}

func formatTimes(times []schedule.HHMM) string {
	parts := make([]string, len(times))
	for i, t := range times {
		parts[i] = fmt.Sprintf("%02d:%02d", t.H, t.M)
	}
	return strings.Join(parts, ",")
}

// defaultEnvPath prefers .env in the working directory, then next to the executable.
func defaultEnvPath() string {
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), ".env")
	}
	return ".env"
}

// alertConfigError tries to reach Telegram even when the configuration is
// broken, as long as the bot settings themselves are present.
func alertConfigError(err error) {
	token, chat := os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID")
	if token == "" || chat == "" {
		return
	}
	host, _ := os.Hostname()
	tg := alert.NewTelegram(token, chat, 2)
	_, _ = tg.Send(fmt.Sprintf("[autodump %s] ❌ cannot start, configuration error:\n%s", host, err))
}

type logOnlySender struct{}

func (logOnlySender) Send(string) (int64, error) { return 0, nil }
func (logOnlySender) Pin(int64) error            { return nil }
