// Package billingwebhook serves a payments provider's webhook endpoint.
//
// Outside the Connect chain by necessity, not merely by preference:
// verification needs the exact raw bytes and Connect hands a handler a decoded
// message; the payload schema is the provider's and evolves without us, so
// protovalidate would 400 valid deliveries into a retry loop; and the caller
// authenticates by HMAC, which none of the four auth modes represents. So this
// does its own body cap and its own error recording.
//
// The handler itself is provider-agnostic and is written once. Only Verify and
// Normalize differ per provider, and both sit behind billing.PaymentProvider.
package billingwebhook

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// PathFor names the provider in the URL deliberately. Sniffing it from the
// headers would mean trying each verifier in turn, which is both a signature
// oracle and unresolvable when two schemes share a header name -- and a cutover
// mounts two routes and retires the first on its own schedule, with no flag day.
func PathFor(provider string) string { return "/webhooks/" + provider }

// Payloads are a few KB; this backstops an unbounded read on an open route.
const maxBodyBytes = 1 << 20

// Dodo times a delivery out at 15s and retries, so finishing later only burns a
// connection.
const handlerTimeout = 10 * time.Second

// Mount registers the endpoint for one provider, and mounts NOTHING when the
// provider cannot verify a signature. Never verify-nothing: with no secret
// configured the provider gets 404s, which is the fail-closed direction.
//
// Registered straight on the mux (the /mcp precedent) rather than through
// server.go's handle(), which records into the Connect-only authz contract and
// would fail assertServedServicesMatch at startup.
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

	ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
	defer cancel()

	// The RAW body: re-serializing a decoded payload changes the bytes and breaks
	// the signature.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		slog.WarnContext(ctx, "failed to read a billing webhook body", slogx.Error(err),
			slog.String("provider", h.provider.Name()))
		w.WriteHeader(http.StatusBadRequest)
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
		slog.ErrorContext(ctx, "failed to handle a billing webhook", slogx.Error(err),
			slog.String("provider", h.provider.Name()), slog.String("webhook_id", delivery.WebhookID))
		// No OTel span out here, so RecordError is a no-op and the log above is the
		// live signal.
		telemetry.RecordError(ctx, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
