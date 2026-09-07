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

// Standard Webhooks (standardwebhooks.com) headers: HMAC-SHA256 over
// "{webhook-id}.{webhook-timestamp}.{raw body}", compared in constant time against
// EACH signature in webhook-signature -- there are several during a rotation.
const (
	headerWebhookID        = "webhook-id"
	headerWebhookTimestamp = "webhook-timestamp"
	headerWebhookSignature = "webhook-signature"
)

// tolerance bounds replay of a captured delivery. Wide enough for the whole retry
// schedule, since a retry reuses its original timestamp and a tighter window would
// 401 every late attempt. The inbox's (provider, webhook_id) key stops a replay.
const tolerance = 24 * time.Hour

// secretPrefix is the conventional prefix on a Standard Webhooks signing secret.
const secretPrefix = "whsec_"

var (
	ErrMissingHeaders = errors.New("dodo: webhook is missing required headers")
	ErrTimestamp      = errors.New("dodo: webhook timestamp is malformed or outside the tolerance window")
	ErrSignature      = errors.New("dodo: webhook signature does not verify")
	ErrEmptySecret    = errors.New("dodo: webhook signing secret is empty")
	// ErrMalformedSecret is a prefixed secret whose body is not base64. Named apart
	// from ErrEmptySecret: a mistyped key is a different fix from an unset one.
	ErrMalformedSecret = errors.New("dodo: webhook signing secret after whsec_ is not valid base64")
)

type verifier struct {
	wh *standardwebhooks.Webhook
	// now is injected so the tolerance window is testable without sleeping.
	now func() time.Time
}

// newVerifier strips the conventional "whsec_" prefix and base64-decodes the rest.
// Only a prefixed secret: a raw 32-char one can be valid base64, and decoding it
// would key the HMAC with 24 wrong bytes.
func newVerifier(secret string) (*verifier, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrEmptySecret
	}
	raw, prefixed := strings.CutPrefix(secret, secretPrefix)
	key := []byte(raw)
	if prefixed {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(decoded) == 0 {
			return nil, ErrMalformedSecret
		}
		key = decoded
	}
	wh, err := standardwebhooks.NewWebhookRaw(key)
	if err != nil {
		return nil, err
	}
	return &verifier{wh: wh, now: time.Now}, nil
}

// Verify authenticates a delivery over the RAW bytes -- re-serializing a decoded
// payload breaks the signature. DeliveredAt is the timestamp this already parsed
// for the replay window, so the CAS guard is never handed an unauthenticated one.
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
		return corebilling.Delivery{}, fmt.Errorf("%w: %w", corebilling.ErrUndecodable, err)
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
