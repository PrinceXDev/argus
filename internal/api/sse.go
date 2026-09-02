package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sse writes Server-Sent Events.
//
// SSE rather than WebSockets: the stream is one-directional, it survives every
// proxy that speaks HTTP/1.1, it reconnects on its own in the browser, and it
// is about twenty lines of Go. A WebSocket would buy bidirectionality that
// nothing here needs.
type sse struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSE(w http.ResponseWriter) (*sse, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("sse: response writer does not support flushing")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold the whole
	// stream until completion and defeat the point of streaming at all.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sse{w: w, flusher: flusher}, nil
}

// send writes one named event. Errors are deliberately swallowed: a client that
// has disconnected mid-stream is an ordinary occurrence, not a server fault,
// and the request context cancellation is what actually stops the work.
func (s *sse) send(event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": "payload could not be encoded"})
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	s.flusher.Flush()
}

func (s *sse) close() {
	fmt.Fprint(s.w, "event: close\ndata: {}\n\n")
	s.flusher.Flush()
}
