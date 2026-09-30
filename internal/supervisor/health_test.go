package supervisor

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumitwaani2/dootd/internal/app"
)

func freePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// The error names the real problem, not a probe cut short by the deadline.
func TestHealthCheckErrors(t *testing.T) {
	p := Policy{HealthTimeout: 400 * time.Millisecond, HealthInterval: 30 * time.Millisecond}

	ln, port := freePort(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/slow":
			time.Sleep(150 * time.Millisecond) // the last probe is cut by the deadline
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	for _, path := range []string{"/wrong", "/slow", "/wrong", "/slow"} {
		err := healthCheck(context.Background(), app.Spec{Port: port, HealthPath: path}, p)
		if err == nil || !strings.Contains(err.Error(), "returned 404") {
			t.Fatalf("health path %s: %v", path, err)
		}
	}
	if err := healthCheck(context.Background(), app.Spec{Port: port, HealthPath: "/healthz"}, p); err != nil {
		t.Fatalf("healthy app: %v", err)
	}

	ln2, closed := freePort(t)
	ln2.Close()
	err := healthCheck(context.Background(), app.Spec{Port: closed, HealthPath: "/"}, p)
	if err == nil || !strings.Contains(err.Error(), "nothing listening") {
		t.Fatalf("closed port: %v", err)
	}
}
