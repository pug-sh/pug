package dodo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

// Standard Webhooks (standardwebhooks.com) headers. The signature is
// HMAC-SHA256 over "{webhook-id}.{webhook-timestamp}.{raw body}", compared in
// constant time against EACH space-delimited signature in webhook-signature --
// the header carries several during a secret rotation.
const (
	headerWebhookID        = "webhook-id"
	headerWebhookTimestamp = "webhook-timestamp"
	headerWebhookSignature = "webhook-signature"
)

// tolerance bounds replay of a captured delivery.
const tolerance = 5 * time.Minute

// secretPrefix is the conventional prefix on a Standard Webhooks signing secret.
const secretPrefix = "whsec_"

var (
	ErrMissingHeaders = errors.New("dodo: webhook is missing required headers")
	ErrTimestamp      = errors.New("dodo: webhook timestamp is malformed or outside the tolerance window")
	ErrSignature      = errors.New("dodo: webhook signature does not verify")
	ErrEmptySecret    = errors.New("dodo: webhook signing secret is empty")
)

type verifier struct {
	wh *standardwebhooks.Webhook
	// now is injected so the tolerance window is testable without sleeping.
	now func() time.Time
}

// newVerifier strips the conventional "whsec_" prefix and base64-decodes the
// rest, falling back to the raw value -- not every deployment mints base64.
func newVerifier(secret string) (*verifier, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrEmptySecret
	}
	raw := strings.TrimPrefix(secret, secretPrefix)
	key := []byte(raw)
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) > 0 {
		key = decoded
	}
	wh, err := standardwebhooks.NewWebhookRaw(key)
	if err != nil {
		return nil, err
	}
	return &verifier{wh: wh, now: time.Now}, nil
}

// Verify authenticates a delivery over the RAW bytes. Re-serializing a decoded
// payload changes them and breaks the signature, so the body must be exactly
// what arrived.
//
// The returned DeliveredAt is the timestamp this already parsed for the replay
// window, which then becomes the CAS guard -- so the guard cannot be handed a
// stamp nothing authenticated.
func (c *Client) Verify(headers http.Header, rawBody []byte) (corebilling.Delivery, error) {
	if c.verifier == nil {
		return corebilling.Delivery{}, ErrEmptySecret
	}
	return c.verifier.verify(headers, rawBody)
}

func (v *verifier) verify(headers http.Header, rawBody []byte) (corebilling.Delivery, error) {
	id := headers.Get(headerWebhookID)
	ts := headers.Get(headerWebhookTimestamp)
	if id == "" || ts == "" || headers.Get(headerWebhookSignature) == "" {
		return corebilling.Delivery{}, ErrMissingHeaders
	}

	delivered, err := parseTimestamp(ts)
	if err != nil {
		return corebilling.Delivery{}, err
	}
	if drift := v.now().Sub(delivered); drift > tolerance || drift < -tolerance {
		return corebilling.Delivery{}, ErrTimestamp
	}

	// Freshness is already enforced above against the injected clock.
	if err := v.wh.VerifyIgnoringTimestamp(rawBody, headers); err != nil {
		return corebilling.Delivery{}, fmt.Errorf("%w: %w", ErrSignature, err)
	}

	// The event type is read from the body, which the signature covers. Reading it
	// from a header would take it from outside the signed envelope.
	eventType, err := envelopeType(rawBody)
	if err != nil {
		return corebilling.Delivery{}, err
	}
	return corebilling.Delivery{
		DeliveredAt: delivered,
		EventType:   eventType,
		RawPayload:  rawBody,
		WebhookID:   id,
	}, nil
}

func parseTimestamp(raw string) (time.Time, error) {
	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q", ErrTimestamp, raw)
	}
	return time.Unix(secs, 0).UTC(), nil
}
