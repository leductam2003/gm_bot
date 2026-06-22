package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// handleWS upgrades to a WebSocket and relays every hub envelope (task + log
// events) to the browser as JSON text frames. Read side is drained so client
// pings/closes are handled.
//
// Origin is validated: by default only same-origin handshakes are accepted (the
// coder/websocket default), which blocks a malicious cross-origin page from
// eavesdropping on the task/log stream. Set ZYPER_WS_ORIGINS (comma list) to allow
// extra origins when serving the dashboard from a different host than the API.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if v := strings.TrimSpace(os.Getenv("ZYPER_WS_ORIGINS")); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				opts.OriginPatterns = append(opts.OriginPatterns, p)
			}
		}
	}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	defer c.CloseNow()

	ch, unsub := s.hub.Subscribe()
	defer unsub()

	ctx := c.CloseRead(r.Context()) // we never read app messages; this watches for close

	for {
		select {
		case <-ctx.Done():
			return
		case env := <-ch:
			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
