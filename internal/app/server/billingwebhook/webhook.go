// Package billingwebhook serves a payments provider's webhook endpoint, outside the
// Connect chain by necessity: verification needs the raw bytes, the payload schema
// is the provider's, and the caller authenticates by HMAC rather than an auth mode.
package billingwebhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// PathFor names the provider in the URL deliberately: sniffing it from the headers
// would mean trying each verifier in turn, which is a signature oracle.
func PathFor(provider string) string { return "/webhooks/" + provider }

// Payloads are a few KB; this backstops an unbounded read on an open route.
const maxBodyBytes = 1 << 20

// Dodo times a delivery out at 15s and retries, so finishing later only burns a
// connection.
const handlerTimeout = 10 * time.Second

// Mount registers the endpoint for one provider, and mounts NOTHING when the
// provider cannot verify a signature -- 404 is the fail-closed direction.
// Registered straight on the mux, like /mcp: handle() is the Connect-only path.
func Mount(mux *http.ServeMux, service *corebilling.Service, provider corebilling.PaymentProvider, canVerify bool) bool {
	if service == nil || provider == nil || !canVerify {
		return false
	}
	h := &handler{provider: provider, service: service}
	path := PathFor(provider.Name())
	mux.Handle(path, h)
	// ServeMux exact-matches a no-trailing-slash pattern and only redirects the
	// other way, so the twin is registered rather than relied on.
	mux.Handle(path+"/", h)
	return true
}

type handler struct {
	provider corebilling.PaymentProvider
	service  *corebilling.Service
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Started before the body read: a budget a slow upload could spend would leave a
	// fine delivery no time to be stored.
	ctx := r.Context()

	// Outside the Connect chain nothing upstream started a span, so RecordError would
	// resolve to the noop one and silence every rejection alert on the money path.
	ctx, span := otel.Tracer("server/billingwebhook").Start(ctx, "billing.webhook")
	defer span.End()

	// net/http recovers per connection, so the process survives either way; this
	// is what keeps the panic in slog and OTLP rather than on bare stderr.
	defer func() {
		if v := recover(); v != nil {
			slog.ErrorContext(ctx, "panic serving a billing webhook",
				slog.Any("panic", v), slog.String("provider", h.provider.Name()))
			telemetry.RecordError(ctx, fmt.Errorf("billingwebhook: panic: %v", v))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	// The RAW body: re-serializing a decoded payload changes the bytes and breaks
	// the signature.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	if err != nil {
		// Only an oversized body is the sender's fault. A truncated read is a reset or a
		// deadline, and 400 would stop the retry of a delivery that never landed.
		status := http.StatusInternalServerError
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusBadRequest
		}
		slog.WarnContext(ctx, "failed to read a billing webhook body", slogx.Error(err),
			slog.String("provider", h.provider.Name()), slog.Int("status", status))
		w.WriteHeader(status)
		return
	}

	delivery, err := h.provider.Verify(r.Header, body)
	if err != nil {
		// An auth failure, not a server fault -- recording it would let a scanner
		// probing an open route fill the error telemetry.
		slog.WarnContext(ctx, "rejected a billing webhook", slogx.Error(err),
			slog.String("provider", h.provider.Name()))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// 2xx only once the row is durable: the provider retries 8 times with
	// exponential backoff, and a 5xx here is what asks for the next attempt.
	if err := h.service.HandleDelivery(ctx, h.provider, delivery); err != nil {
		// Disposition only: HandleDelivery logs and records at the layer that detected the
		// fault, so re-recording here doubles the count on the span above.
		slog.WarnContext(ctx, "asking the provider to retry a billing webhook", slogx.Error(err),
			slog.String("provider", h.provider.Name()), slog.String("webhook_id", delivery.WebhookID))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
