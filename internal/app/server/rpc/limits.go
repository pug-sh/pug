package rpc

import (
	"net/http"
	"time"
)

// Sized off measured batches: a full 1000-event batch is ~1 MiB, a fat one ~4 MiB.
const MaxRequestBytes = 8 << 20

// Generous on purpose — dropping real events costs more than holding a connection.
const bodyReadTimeout = 2 * time.Minute

// WithRequestLimits caps the request body and how long a client may take to upload
// it. Mount it around the whole mux so non-Connect routes (/mcp) are covered too.
func WithRequestLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		// net/http clears this once the body hits EOF, so a streaming response outlives it.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
		next.ServeHTTP(w, r)
	})
}
