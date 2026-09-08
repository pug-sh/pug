// Package webhook serves provider callbacks outside the Connect chain:
// verification needs the raw bytes, and the caller authenticates by HMAC rather
// than an auth mode.
package webhook

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

// The provider is named in the URL because sniffing it from the headers means
// trying each verifier in turn: a signature oracle.
func BillingPath(provider string) string { return "/billing/webhooks/" + provider }

// Payloads are a few KB; the cap is a backstop on an open route.
const maxBodyBytes = 1 << 20

// Dodo times a delivery out at 15s and retries, so finishing later only burns a
// connection.
const billingHandlerTimeout = 10 * time.Second

// MountBilling skips a provider that cannot verify a signature: 404 is the
// fail-closed direction.
func MountBilling(mux *http.ServeMux, service *corebilling.Service, provider corebilling.PaymentProvider) bool {
	if service == nil || provider == nil || !provider.CanVerify() {
		return false
	}
	h := &billingHandler{provider: provider, service: service}
	path := BillingPath(provider.Name())
	mux.Handle(path, h)
	// ServeMux only redirects toward the trailing slash, so the twin is registered
	// rather than relied on.
	mux.Handle(path+"/", h)
	return true
}

type billingHandler struct {
	provider corebilling.PaymentProvider
	service  *corebilling.Service
}

func (h *billingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Nothing upstream starts a span here, and RecordError against the noop span
	// would silence every rejection on the money path.
	ctx, span := otel.Tracer("server/webhook").Start(ctx, "billing.webhook")
	defer span.End()

	// net/http already recovers per connection; this is what puts the panic in slog
	// and OTLP rather than on bare stderr.
	defer func() {
		if v := recover(); v != nil {
			slog.ErrorContext(ctx, "panic serving a billing webhook",
				slog.Any("panic", v), slog.String("provider", h.provider.Name()))
			telemetry.RecordError(ctx, fmt.Errorf("webhook: panic: %v", v))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()

	// The RAW body: re-serializing a decoded payload breaks the signature. The
	// timeout starts after it, or a slow upload spends a good delivery's budget.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	ctx, cancel := context.WithTimeout(ctx, billingHandlerTimeout)
	defer cancel()
	if err != nil {
		// Only an oversized body is the sender's fault: a truncated read is a reset,
		// and 400 would stop the retry of a delivery that never landed.
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			slog.WarnContext(ctx, "rejected an oversized billing webhook body", slogx.Error(err),
				slog.String("provider", h.provider.Name()))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// This is the detecting layer, and a systematic read failure burns every retry
		// and then leaves the delivery stranded with no reason recorded anywhere.
		slog.ErrorContext(ctx, "failed to read a billing webhook body", slogx.Error(err),
			slog.String("provider", h.provider.Name()))
		telemetry.RecordError(ctx, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	delivery, err := h.provider.Verify(r.Header, body)
	if err != nil {
		// Verified but unreadable is pug's fault: 401 would file the money path going
		// down under the warning a port scanner produces.
		if errors.Is(err, corebilling.ErrUndecodable) {
			slog.ErrorContext(ctx, "cannot decode a verified billing webhook", slogx.Error(err),
				slog.String("provider", h.provider.Name()))
			telemetry.RecordError(ctx, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// An auth failure, not a server fault: recording it lets a scanner probing an
		// open route fill the error telemetry.
		slog.WarnContext(ctx, "rejected a billing webhook", slogx.Error(err),
			slog.String("provider", h.provider.Name()))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Answered only once the row is durable, since a 5xx is what asks for a retry.
	if err := h.service.HandleDelivery(ctx, h.provider, delivery); err != nil {
		// Disposition only: HandleDelivery already logged and recorded it.
		slog.WarnContext(ctx, "asking the provider to retry a billing webhook", slogx.Error(err),
			slog.String("provider", h.provider.Name()), slog.String("webhook_id", delivery.WebhookID))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
