package alert

import (
	"log"
	"sort"
	"sync"
)

// Notifier is what the exporter reports to.
type Notifier interface {
	Info(text string)
	Failure(key, text string)
	Stall(docID uint64, text string)
	StallResolved(docID uint64, text string)
}

// Alerter deduplicates notifications and logs every one of them,
// so an alert is never lost even when Telegram is unreachable.
type Alerter struct {
	sender Sender
	log    *log.Logger
	prefix string

	mu       sync.Mutex
	failures map[string]bool // reset each run
	stalls   map[uint64]bool // lives for the whole process
}

// New wraps a sender. name identifies this instance in messages, e.g. the host name.
func New(sender Sender, logger *log.Logger, name string) *Alerter {
	return &Alerter{
		sender:   sender,
		log:      logger,
		prefix:   "[autodump " + name + "] ",
		failures: map[string]bool{},
		stalls:   map[uint64]bool{},
	}
}

// ResetRun forgets failure keys so the next run can report them again.
func (a *Alerter) ResetRun() {
	a.mu.Lock()
	a.failures = map[string]bool{}
	a.mu.Unlock()
}

// Info sends an informational message.
func (a *Alerter) Info(text string) {
	a.log.Printf("INFO alert: %s", text)
	a.send(text, false)
}

// Failure reports a problem once per key per run.
func (a *Alerter) Failure(key, text string) {
	a.mu.Lock()
	seen := a.failures[key]
	a.failures[key] = true
	a.mu.Unlock()

	a.log.Printf("ERROR alert [%s]: %s", key, text)
	if !seen {
		a.send("❌ "+text, false)
	}
}

// Stall reports a stuck document once and pins the message.
func (a *Alerter) Stall(docID uint64, text string) {
	a.mu.Lock()
	seen := a.stalls[docID]
	a.stalls[docID] = true
	a.mu.Unlock()

	if seen {
		return
	}
	a.log.Printf("WARN stall alert [doc %d]: %s", docID, text)
	a.send("⏳ "+text, true)
}

// StallResolved reports that a previously reported stuck document moved on.
func (a *Alerter) StallResolved(docID uint64, text string) {
	a.mu.Lock()
	seen := a.stalls[docID]
	delete(a.stalls, docID)
	a.mu.Unlock()

	if !seen {
		return
	}
	a.log.Printf("INFO stall resolved [doc %d]: %s", docID, text)
	a.send("✅ "+text, false)
}

// ReportedStalls lists documents with an open stall alert, ascending.
func (a *Alerter) ReportedStalls() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]uint64, 0, len(a.stalls))
	for id := range a.stalls {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (a *Alerter) send(text string, pin bool) {
	id, err := a.sender.Send(a.prefix + text)
	if err != nil {
		a.log.Printf("ERROR telegram send failed: %v", err)
		return
	}
	if pin {
		if err := a.sender.Pin(id); err != nil {
			a.log.Printf("WARN telegram pin failed (message was sent): %v", err)
		}
	}
}
