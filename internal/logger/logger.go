// Package logger is the structured, redacting, rotating log sink. Every entry is
// written as a JSON line to logs/<category>.log, kept in an in-memory ring buffer
// (for the Logs page initial load), and published to the events hub for live WS
// streaming. Secrets (full private keys / signatures) are redacted before they
// ever reach a file or the wire.
package logger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"zyperbot/internal/events"
)

type Level string

const (
	DEBUG Level = "DEBUG"
	INFO  Level = "INFO"
	WARN  Level = "WARN"
	ERROR Level = "ERROR"
)

// Category routes an entry to a file and lets the UI filter.
type Category string

const (
	CatTx   Category = "tx"
	CatTask Category = "task"
	CatErr  Category = "error"
	CatAPI  Category = "api"
)

// Entry is one structured log line.
type Entry struct {
	Time     time.Time      `json:"time"`
	Level    Level          `json:"level"`
	Category Category       `json:"category"`
	Msg      string         `json:"msg"`
	TaskID   int64          `json:"taskId,omitempty"`
	Wallet   string         `json:"wallet,omitempty"` // already shortened by caller or redaction
	Fields   map[string]any `json:"fields,omitempty"`
}

const (
	maxFileBytes = 10 << 20 // 10 MiB → rotate to .old
	ringSize     = 1000
)

type Logger struct {
	dir      string
	hub      *events.Hub
	minLevel Level

	mu    sync.Mutex
	files map[Category]*os.File
	ring  []Entry
	rhead int
	rfull bool
}

// New opens (creating if needed) the log directory.
func New(dir string, hub *events.Hub) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Logger{
		dir: dir, hub: hub, minLevel: INFO,
		files: map[Category]*os.File{},
		ring:  make([]Entry, ringSize),
	}, nil
}

func (l *Logger) SetLevel(lv Level) { l.mu.Lock(); l.minLevel = lv; l.mu.Unlock() }

var levelRank = map[Level]int{DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3}

// --- redaction ---

// Long hex blobs are the danger: a 32-byte private key (0x + 64 hex) or a 65-byte
// signature (0x + 130 hex). Shorten any 0x-hex of length >= 40 chars to 0x1234…abcd.
var longHex = regexp.MustCompile(`0x[0-9a-fA-F]{40,}`)

func redact(s string) string {
	return longHex.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) <= 12 {
			return m
		}
		return m[:6] + "…" + m[len(m)-4:]
	})
}

func redactFields(f map[string]any) map[string]any {
	if f == nil {
		return nil
	}
	out := make(map[string]any, len(f))
	for k, v := range f {
		if sv, ok := v.(string); ok {
			out[k] = redact(sv)
		} else {
			out[k] = v
		}
	}
	return out
}

// Short turns an address/hash into 0x1234…abcd for display.
func Short(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// --- emit ---

func (l *Logger) log(e Entry) {
	l.mu.Lock()
	if levelRank[e.Level] < levelRank[l.minLevel] {
		l.mu.Unlock()
		return
	}
	e.Msg = redact(e.Msg)
	e.Fields = redactFields(e.Fields)
	if e.Wallet != "" {
		e.Wallet = Short(e.Wallet)
	}
	// ring buffer
	l.ring[l.rhead] = e
	l.rhead = (l.rhead + 1) % ringSize
	if l.rhead == 0 {
		l.rfull = true
	}
	l.writeFile(e)
	l.mu.Unlock()

	if l.hub != nil {
		l.hub.Publish("log", e)
	}
}

// writeFile appends the JSON line to the category file (caller holds the lock).
func (l *Logger) writeFile(e Entry) {
	f := l.fileFor(e.Category)
	if f == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err == nil {
		l.maybeRotate(e.Category, f)
	}
}

func (l *Logger) fileFor(cat Category) *os.File {
	if f, ok := l.files[cat]; ok {
		return f
	}
	path := filepath.Join(l.dir, string(cat)+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil
	}
	l.files[cat] = f
	return f
}

func (l *Logger) maybeRotate(cat Category, f *os.File) {
	st, err := f.Stat()
	if err != nil || st.Size() < maxFileBytes {
		return
	}
	f.Close()
	delete(l.files, cat)
	base := filepath.Join(l.dir, string(cat)+".log")
	_ = os.Rename(base, base+".old") // keep one previous generation
}

// Snapshot returns the ring buffer oldest→newest (for the Logs page initial load).
func (l *Logger) Snapshot() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	if l.rfull {
		out = append(out, l.ring[l.rhead:]...)
	}
	out = append(out, l.ring[:l.rhead]...)
	return out
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		f.Close()
	}
}

// --- public helpers ---

func (l *Logger) Tx(level Level, msg string, taskID int64, wallet string, fields map[string]any) {
	l.log(Entry{Time: time.Now(), Level: level, Category: CatTx, Msg: msg, TaskID: taskID, Wallet: wallet, Fields: fields})
}
func (l *Logger) Task(level Level, msg string, taskID int64, fields map[string]any) {
	l.log(Entry{Time: time.Now(), Level: level, Category: CatTask, Msg: msg, TaskID: taskID, Fields: fields})
}
func (l *Logger) Errorf(msg string, fields map[string]any) {
	l.log(Entry{Time: time.Now(), Level: ERROR, Category: CatErr, Msg: msg, Fields: fields})
}
func (l *Logger) API(level Level, msg string, fields map[string]any) {
	l.log(Entry{Time: time.Now(), Level: level, Category: CatAPI, Msg: msg, Fields: fields})
}
