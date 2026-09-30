package web

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxLogPage = 2 << 20

// sseWriter writes Server-Sent Events.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// send writes one event; data may contain several lines.
func (s *sseWriter) send(event, data string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	for _, l := range strings.Split(data, "\n") {
		b.WriteString("data: " + l + "\n")
	}
	b.WriteString("\n")
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) ping() error {
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		return err
	}
	return s.rc.Flush()
}

func readLog(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "(no deploy log)"
	}
	defer f.Close()
	st, _ := f.Stat()
	if st != nil && st.Size() > maxLogPage {
		f.Seek(-maxLogPage, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// deploymentStream tails a deploy log until the deployment finishes, then
// sends a "done" event with the final status.
func (s *Server) deploymentStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.Dep.Deployment(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := s.Layout.BuildLog(d.App, d.ID)
	sse := newSSE(w)
	var off int64
	var partial string
	buf := make([]byte, 64<<10)
	lastPing := time.Now()
	for {
		cur, err := s.Dep.Deployment(r.Context(), id)
		done := err != nil || cur.Done()
		if f, err := os.Open(path); err == nil {
			f.Seek(off, io.SeekStart)
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					off += int64(n)
					chunk := partial + string(buf[:n])
					lines := strings.Split(chunk, "\n")
					partial = lines[len(lines)-1]
					if len(lines) > 1 {
						if err := sse.send("", strings.Join(lines[:len(lines)-1], "\n")); err != nil {
							f.Close()
							return
						}
					}
				}
				if rerr != nil {
					break
				}
			}
			f.Close()
		}
		if done {
			if partial != "" {
				sse.send("", partial)
			}
			sse.send("done", cur.Status)
			return
		}
		if time.Since(lastPing) > 15*time.Second {
			if sse.ping() != nil {
				return
			}
			lastPing = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// logsStream sends recent app log lines, then new ones as they arrive.
func (s *Server) logsStream(w http.ResponseWriter, r *http.Request) {
	a := s.Sup.Get(r.PathValue("app"))
	if a == nil {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 300
	}
	ch, cancel := a.Log().Subscribe(512)
	defer cancel()
	sse := newSSE(w)
	var b strings.Builder
	for _, l := range a.Log().Recent(n) {
		b.WriteString(l.String() + "\n")
	}
	if b.Len() > 0 {
		if sse.send("", strings.TrimSuffix(b.String(), "\n")) != nil {
			return
		}
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if sse.ping() != nil {
				return
			}
		case l, ok := <-ch:
			if !ok {
				sse.send("done", "log closed")
				return
			}
			if sse.send("", l.String()) != nil {
				return
			}
		}
	}
}
