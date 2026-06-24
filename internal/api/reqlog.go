package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"zyperbot/internal/logger"
)

// logRW wraps an http.ResponseWriter to capture the status code and, for error responses,
// a capped copy of the JSON body — so the request logger can report the exact error a
// handler returned, not just the status. Hijack/Flush are delegated so a WebSocket upgrade
// or streaming handler keeps working even if this ever wraps one.
type logRW struct {
	http.ResponseWriter
	status  int
	errBody []byte
}

func (w *logRW) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logRW) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.errBody) < 512 { // capture the error message body
		room := 512 - len(w.errBody)
		if room > len(b) {
			room = len(b)
		}
		w.errBody = append(w.errBody, b[:room]...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *logRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
}

func (w *logRW) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestLog records every API request in the Logs tab: method + path, status code, and
// duration — with the handler's error message captured on 4xx/5xx so failures are fully
// visible. High-frequency polls (gas, status, the logs snapshot itself) log at DEBUG so
// they don't drown the stream; everything else is INFO, client errors WARN, server
// errors ERROR.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &logRW{ResponseWriter: w}
		next.ServeHTTP(lw, r)
		if lw.status == 0 {
			lw.status = http.StatusOK
		}
		path := r.URL.Path
		fields := map[string]any{"status": lw.status, "ms": time.Since(start).Milliseconds()}

		level := logger.INFO
		switch {
		case lw.status >= 500:
			level = logger.ERROR
		case lw.status >= 400:
			level = logger.WARN
		case r.Method == http.MethodGet && isNoisyPath(path):
			level = logger.DEBUG
		}
		if len(lw.errBody) > 0 {
			fields["error"] = strings.TrimSpace(string(lw.errBody))
		}
		s.log.API(level, r.Method+" "+path, fields)
	})
}

// isNoisyPath is the small set of frequently-polled read endpoints that log at DEBUG
// (hidden unless the user selects the DEBUG level) so they don't flood the Logs tab.
func isNoisyPath(p string) bool {
	return strings.HasSuffix(p, "/gas") || strings.HasSuffix(p, "/status") || strings.HasSuffix(p, "/logs")
}
