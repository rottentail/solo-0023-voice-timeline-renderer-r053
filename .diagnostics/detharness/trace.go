package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Trace is a mutex-protected ordered event log shared by both simulated
// service instances. It records the actual interleaving observed in the
// shared process so each scenario run is fully replayable.
type Trace struct {
	mu     sync.Mutex
	events []string
	dir    string
}

func NewTrace(dir string) *Trace {
	return &Trace{dir: dir}
}

func (t *Trace) Event(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	stamp := time.Now().UTC().Format("15:04:05.000000")
	t.events = append(t.events, stamp+" "+msg)
}

func (t *Trace) Eventf(format string, args ...any) {
	t.Event(fmt.Sprintf(format, args...))
}

func (t *Trace) writeFile(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	payload := ""
	for _, event := range t.events {
		payload += event + "\n"
	}
	return os.WriteFile(filepath.Join(t.dir, name), []byte(payload), 0o644)
}
