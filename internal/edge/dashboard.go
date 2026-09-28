package edge

import "net/http"

// PlaceholderDashboard answers on the dashboard domain until the real
// dashboard exists (Phase 4). It reveals nothing about the host.
func PlaceholderDashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if r.URL.Path != "/" {
			page(w, http.StatusNotFound, "Not found", "Nothing here yet.", 0)
			return
		}
		page(w, http.StatusOK, "dootd", "dootd is running. The dashboard arrives in a later version.", 0)
	})
}
