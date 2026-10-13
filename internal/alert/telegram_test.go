package alert

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeAPI struct {
	mu       sync.Mutex
	sent     []string
	pinned   []int64
	failSend int // number of sendMessage calls to fail before succeeding
	failPin  bool
	nextID   int64
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("chat_id") != "42" {
			t.Errorf("chat_id = %q", r.Form.Get("chat_id"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if f.failSend > 0 {
				f.failSend--
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"ok":false,"description":"bad gateway"}`)
				return
			}
			f.nextID++
			f.sent = append(f.sent, r.Form.Get("text"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": f.nextID}})
		case strings.HasSuffix(r.URL.Path, "/pinChatMessage"):
			if f.failPin {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"description":"not enough rights"}`)
				return
			}
			var id int64
			_ = json.Unmarshal([]byte(r.Form.Get("message_id")), &id)
			f.pinned = append(f.pinned, id)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
}

func newTestAlerter(t *testing.T, f *fakeAPI) (*Alerter, *strings.Builder) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	tg := NewTelegram("SECRET-TOKEN", "42", 3)
	tg.BaseURL = srv.URL
	tg.Backoff = 0
	var logs strings.Builder
	return New(tg, log.New(&logs, "", 0), "test"), &logs
}

func TestFailureDedupedPerRun(t *testing.T) {
	f := &fakeAPI{}
	a, _ := newTestAlerter(t, f)

	a.Failure("db:index", "db down")
	a.Failure("db:index", "db down again")
	a.Failure("io:x", "disk full")
	if len(f.sent) != 2 {
		t.Fatalf("sent %d messages, want 2: %q", len(f.sent), f.sent)
	}
	a.ResetRun()
	a.Failure("db:index", "db down next run")
	if len(f.sent) != 3 {
		t.Fatalf("after ResetRun sent %d, want 3", len(f.sent))
	}
	if !strings.Contains(f.sent[0], "[autodump test]") {
		t.Errorf("missing prefix: %q", f.sent[0])
	}
}

func TestStallPinnedOnceAndResolved(t *testing.T) {
	f := &fakeAPI{}
	a, _ := newTestAlerter(t, f)

	a.Stall(98, "doc 98 stuck")
	a.Stall(98, "doc 98 stuck")
	if len(f.sent) != 1 || len(f.pinned) != 1 || f.pinned[0] != 1 {
		t.Fatalf("sent=%d pinned=%v, want 1 message pinned id 1", len(f.sent), f.pinned)
	}
	if got := a.ReportedStalls(); len(got) != 1 || got[0] != 98 {
		t.Fatalf("ReportedStalls = %v", got)
	}
	a.StallResolved(98, "doc 98 resolved")
	a.StallResolved(98, "doc 98 resolved")
	if len(f.sent) != 2 {
		t.Fatalf("sent=%d, want 2 (one resolved)", len(f.sent))
	}
	if len(a.ReportedStalls()) != 0 {
		t.Fatal("stall not cleared")
	}
	// Survives ResetRun: stalls are process-lifetime.
	a.Stall(99, "doc 99 stuck")
	a.ResetRun()
	a.Stall(99, "doc 99 stuck")
	if len(f.sent) != 3 {
		t.Fatalf("sent=%d, want 3", len(f.sent))
	}
}

func TestStallResolvedWithoutReportIsNoop(t *testing.T) {
	f := &fakeAPI{}
	a, _ := newTestAlerter(t, f)
	a.StallResolved(5, "x")
	if len(f.sent) != 0 {
		t.Fatalf("sent %d", len(f.sent))
	}
}

func TestPinFailureStillSendsAndLogs(t *testing.T) {
	f := &fakeAPI{failPin: true}
	a, logs := newTestAlerter(t, f)
	a.Stall(7, "stuck")
	if len(f.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(f.sent))
	}
	if !strings.Contains(logs.String(), "pin") {
		t.Errorf("pin failure not logged: %q", logs.String())
	}
}

func TestSendRetriesThenSucceeds(t *testing.T) {
	f := &fakeAPI{failSend: 2}
	a, _ := newTestAlerter(t, f)
	a.Info("hello")
	if len(f.sent) != 1 {
		t.Fatalf("sent %d, want 1 after retries", len(f.sent))
	}
}

func TestSendGivesUpAndRedactsToken(t *testing.T) {
	f := &fakeAPI{failSend: 10}
	a, logs := newTestAlerter(t, f)
	a.Failure("k", "boom")
	if len(f.sent) != 0 {
		t.Fatalf("sent %d", len(f.sent))
	}
	out := logs.String()
	if !strings.Contains(out, "boom") {
		t.Errorf("alert text not logged: %q", out)
	}
	if strings.Contains(out, "SECRET-TOKEN") {
		t.Errorf("token leaked into log: %q", out)
	}
}

func TestUnreachableServerRedactsToken(t *testing.T) {
	tg := NewTelegram("SECRET-TOKEN", "42", 1)
	tg.BaseURL = "http://127.0.0.1:1" // nothing listens here
	tg.Backoff = 0
	_, err := tg.Send("x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("token leaked: %v", err)
	}
}

func TestLongMessageTruncated(t *testing.T) {
	f := &fakeAPI{}
	a, _ := newTestAlerter(t, f)
	a.Info(strings.Repeat("я", 5000))
	if n := len([]rune(f.sent[0])); n > maxMessageRunes {
		t.Fatalf("message has %d runes, max %d", n, maxMessageRunes)
	}
}
