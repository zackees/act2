package serve

import (
	"encoding/json"
	"net/http"
	"sync"
)

// frameWriter streams an exec's output as NDJSON frames. A client that goes
// away does not stop act: writes after the first failure are dropped.
type frameWriter struct {
	mu      sync.Mutex
	out     http.ResponseWriter
	flusher http.Flusher
	enc     *json.Encoder
	broken  bool
}

func newFrameWriter(w http.ResponseWriter) *frameWriter {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return &frameWriter{out: w, flusher: flusher, enc: json.NewEncoder(w)}
}

func (f *frameWriter) send(frame Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return
	}
	if err := f.enc.Encode(frame); err != nil {
		f.broken = true
		return
	}
	if f.flusher != nil {
		f.flusher.Flush()
	}
}

func (f *frameWriter) exit(end Exit) { f.send(Frame{Exit: &end}) }

type streamWriter struct {
	frames *frameWriter
	name   string
}

func (s streamWriter) Write(p []byte) (int, error) {
	s.frames.send(Frame{Stream: s.name, Data: append([]byte(nil), p...)})
	return len(p), nil
}

func (f *frameWriter) writer(name string) streamWriter { return streamWriter{frames: f, name: name} }
