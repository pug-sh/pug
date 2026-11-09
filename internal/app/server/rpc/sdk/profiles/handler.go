package profiles

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	"github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	sdkprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/sdk/profiles/v1/sdkprofilesv1connect"
	"github.com/pug-sh/pug/internal/geo"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxCoalescedBytes caps a merged identify so it still fits NATS's default 1 MiB
// max_payload, with room left for the envelope and trace headers.
const maxCoalescedBytes = 900 << 10

var (
	ipStrippedCounter      metric.Int64Counter
	nulTraitDroppedCounter metric.Int64Counter
	invalidSkippedCounter  metric.Int64Counter
)

func init() {
	meter := otel.Meter("github.com/pug-sh/pug/internal/app/server/rpc/sdk/profiles")
	ipStrippedCounter, _ = meter.Int64Counter(
		"profiles.identify_ip_stripped_total",
		metric.WithDescription("A $ip trait was stripped during Identify or BatchIdentify because the visitor IP must never be persisted. Our SDKs never add it, so a non-zero count means a caller supplied an IP in traits."),
	)
	nulTraitDroppedCounter, _ = meter.Int64Counter(
		"profiles.identify_nul_trait_dropped_total",
		metric.WithDescription("A trait was dropped during Identify or BatchIdentify because its key, or a string in it, held a NUL, which Postgres can't store. The rest of the identify still applies."),
	)
	invalidSkippedCounter, _ = meter.Int64Counter(
		"profiles.batch_identify_invalid_skipped_total",
		metric.WithDescription("A BatchIdentify item broke a validation rule and was skipped, keeping the rest of its batch (listed in invalid_identifies). rule is the first rule it broke. It is skipped again on every retry."),
	)
}

type Server struct {
	sdkprofilesv1connect.UnimplementedProfilesSDKServiceHandler
	producer jetstream.JetStream
}

func NewServer(js jetstream.JetStream) *Server {
	return &Server{
		producer: js,
	}
}

func (s *Server) Identify(
	ctx context.Context,
	req *connect.Request[sdkprofilesv1.IdentifyRequest],
) (*connect.Response[sdkprofilesv1.IdentifyResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	dropNULTraits(ctx, principal.Project.ID, req.Msg.GetTraits())
	if err := s.publishIdentify(ctx, principal.Project.ID, req.Msg); err != nil {
		return nil, err
	}

	return connect.NewResponse(&sdkprofilesv1.IdentifyResponse{}), nil
}

// BatchIdentify skips invalid items, merges what it can with coalesce, then
// publishes each message as Identify publishes one, stopping at the first failure.
// It isn't atomic: a client retrying the batch repeats whatever was already
// published.
func (s *Server) BatchIdentify(
	ctx context.Context,
	req *connect.Request[sdkprofilesv1.BatchIdentifyRequest],
) (*connect.Response[sdkprofilesv1.BatchIdentifyResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	items, invalid := skipInvalid(ctx, principal.Project.ID, req.Msg.GetIdentifies())
	// Per item, before merging: a later item's dropped trait must not erase an
	// earlier item's value for the same key.
	for _, item := range items {
		dropNULTraits(ctx, principal.Project.ID, item.GetTraits())
	}
	messages := coalesce(items)
	for i, message := range messages {
		if err := s.publishIdentify(ctx, principal.Project.ID, message); err != nil {
			slog.WarnContext(ctx, "batch identify stopped partway",
				slog.Int("published", i), slog.Int("messages", len(messages)), slog.Int("items", len(items)),
				slog.String("project_id", principal.Project.ID))
			return nil, err
		}
	}

	return connect.NewResponse(&sdkprofilesv1.BatchIdentifyResponse{
		// Items, not messages: fewer than sent would read as refused.
		Accepted:          proto.Uint32(uint32(len(items))),
		InvalidIdentifies: invalid,
	}), nil
}

// coalesce merges each item that only sets traits into that user's latest message.
// The identify worker runs concurrently, so separate messages for one user can apply
// in either order; one message gives the result of applying them in order.
//
// Two kinds of item stay out of a merge. One carrying anonymous_id or device_id stays
// where it is, because those act across users and moving it could change the outcome.
// One that would take a message past maxCoalescedBytes starts a new message, which later
// items join.
func coalesce(items []*sdkprofilesv1.IdentifyRequest) []*sdkprofilesv1.IdentifyRequest {
	type latest struct {
		message *sdkprofilesv1.IdentifyRequest
		size    int
	}
	messages := make([]*sdkprofilesv1.IdentifyRequest, 0, len(items))
	byUser := make(map[string]*latest, len(items))
	for _, item := range items {
		size := proto.Size(item)
		onlySetsTraits := item.GetAnonymousId() == "" && item.GetDeviceId() == ""
		last, seen := byUser[item.GetExternalId()]
		if seen && onlySetsTraits && last.size+size <= maxCoalescedBytes {
			mergeTraits(last.message, item)
			last.size += size
			continue
		}
		byUser[item.GetExternalId()] = &latest{message: item, size: size}
		messages = append(messages, item)
	}
	return messages
}

// mergeTraits applies from's traits over into's, later values winning per top-level
// key, as jsonb_shallow_merge does when the worker applies them.
func mergeTraits(into, from *sdkprofilesv1.IdentifyRequest) {
	if into.GetTraits().GetFields() == nil {
		into.Traits = from.GetTraits()
		return
	}
	maps.Copy(into.Traits.Fields, from.GetTraits().GetFields())
}

// skipInvalid validates each item on its own, so one that breaks a rule is skipped
// and reported instead of failing the whole batch.
func skipInvalid(ctx context.Context, projectID string, items []*sdkprofilesv1.IdentifyRequest) ([]*sdkprofilesv1.IdentifyRequest, []*sdkprofilesv1.InvalidIdentify) {
	kept := make([]*sdkprofilesv1.IdentifyRequest, 0, len(items))
	var invalid []*sdkprofilesv1.InvalidIdentify
	for i, item := range items {
		rule, violations := rpc.Violations(protovalidate.Validate(item))
		if violations == nil {
			kept = append(kept, item)
			continue
		}
		invalid = append(invalid, &sdkprofilesv1.InvalidIdentify{
			Index:      proto.Uint32(uint32(i)),
			Violations: violations,
		})
		invalidSkippedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("project_id", projectID),
			attribute.String("rule", rule),
		))
	}
	return kept, invalid
}

// dropNULTraits drops each trait whose key, or any string in it, holds a NUL: jsonb
// can't store one, and the worker would fail the whole identify on it.
func dropNULTraits(ctx context.Context, projectID string, traits *structpb.Struct) {
	for key, value := range traits.GetFields() {
		if strings.ContainsRune(key, 0) || valueHasNUL(value) {
			delete(traits.Fields, key)
			nulTraitDroppedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("project_id", projectID)))
		}
	}
}

// traitsHaveNUL reports a NUL in any key or string, at any depth. It is Go rather than
// a proto rule because CEL can't recurse into a Struct.
func traitsHaveNUL(traits *structpb.Struct) bool {
	for key, value := range traits.GetFields() {
		if strings.ContainsRune(key, 0) || valueHasNUL(value) {
			return true
		}
	}
	return false
}

func valueHasNUL(value *structpb.Value) bool {
	switch kind := value.GetKind().(type) {
	case *structpb.Value_StringValue:
		return strings.ContainsRune(kind.StringValue, 0)
	case *structpb.Value_StructValue:
		return traitsHaveNUL(kind.StructValue)
	case *structpb.Value_ListValue:
		return slices.ContainsFunc(kind.ListValue.GetValues(), valueHasNUL)
	}
	return false
}

// publishIdentify strips a client-supplied $ip trait (counting it) and publishes
// one ProfileIdentifyMessage for the identify worker. Its error is client-safe.
func (s *Server) publishIdentify(ctx context.Context, projectID string, req *sdkprofilesv1.IdentifyRequest) error {
	// The visitor IP is personal data and must never be persisted: strip the
	// canonical $ip key from traits so an untrusted SDK caller cannot inject it
	// into a profile's stored properties (mirrors the events enrichGeo strip),
	// and count any occurrence. Scope is the canonical key; arbitrary client
	// data under other keys is never interpreted as an IP.
	// delete on a nil Fields map (traits unset) is a safe no-op.
	traits := req.GetTraits().GetFields()
	if _, ok := traits[geo.PropIP]; ok {
		ipStrippedCounter.Add(ctx, 1)
	}
	delete(traits, geo.PropIP)

	msg := &sdkprofilesv1.ProfileIdentifyMessage{
		ExternalId:  proto.String(req.GetExternalId()),
		Traits:      req.GetTraits(),
		ProjectId:   proto.String(projectID),
		AnonymousId: proto.String(req.GetAnonymousId()),
		DeviceId:    proto.String(req.GetDeviceId()),
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		slog.ErrorContext(ctx, "failed to marshal identify message", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	if _, err = s.producer.Publish(ctx, nats.ProfileIdentifySubject, data); err != nil {
		// No retry can publish this: answer with a code a client won't resend.
		if errors.Is(err, natsgo.ErrMaxPayload) {
			slog.WarnContext(ctx, "identify too large to publish", slog.Int("bytes", len(data)),
				slog.String("project_id", projectID))
			return apperr.Invalid(apperr.ReasonIdentifyTooLarge, "identify is too large")
		}
		slog.ErrorContext(ctx, "failed to publish identify message", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return connect.NewError(connect.CodeInternal, errors.New("failed to process request"))
	}

	return nil
}
