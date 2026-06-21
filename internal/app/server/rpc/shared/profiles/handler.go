package profiles

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	service *coreprofiles.Service
}

func NewServer(service *coreprofiles.Service) *Server {
	if service == nil {
		panic("profiles: service is nil")
	}
	return &Server{
		service: service,
	}
}

// Delete enqueues erasure of the data subject identified by profile id. The
// PostgreSQL profile is soft-deleted synchronously; the irreversible hard
// erasure (events, rollups, and the ClickHouse profile) runs in the compliance
// worker. Returns the request id for tracking.
func (s *Server) Delete(
	ctx context.Context,
	req *connect.Request[profilesv1.DeleteRequest],
) (*connect.Response[profilesv1.DeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	profileID := req.Msg.GetId()
	requestID, status, err := s.service.RequestErasureByID(ctx, principal.Project.ID, profileID, requestedBy(principal))
	if err != nil {
		if errors.Is(err, coreprofiles.ErrProfileNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProfileNotFound, "profile not found", apperr.Resource("profile", profileID))
		}
		// Already logged + recorded at the service layer; only translate for the client.
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}

	return connect.NewResponse(&profilesv1.DeleteResponse{
		RequestId: proto.String(requestID),
		Status:    complianceStatusToProto(status).Enum(),
	}), nil
}

// DeleteDataSubject enqueues erasure of the data subject identified by
// external_id. Unlike Delete, it succeeds even when no profile row exists, since
// events can be keyed directly by external_id.
func (s *Server) DeleteDataSubject(
	ctx context.Context,
	req *connect.Request[profilesv1.DeleteDataSubjectRequest],
) (*connect.Response[profilesv1.DeleteDataSubjectResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	externalID := req.Msg.GetExternalId()
	requestID, status, err := s.service.RequestErasureByExternalID(ctx, principal.Project.ID, externalID, requestedBy(principal))
	if err != nil {
		// Already logged + recorded at the service layer; only translate for the client.
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to erase data subject"))
	}

	return connect.NewResponse(&profilesv1.DeleteDataSubjectResponse{
		RequestId: proto.String(requestID),
		Status:    complianceStatusToProto(status).Enum(),
	}), nil
}

// GetDeletionRequest returns the status of an erasure request (the DSAR audit
// trail) so a controller can prove fulfilment.
func (s *Server) GetDeletionRequest(
	ctx context.Context,
	req *connect.Request[profilesv1.GetDeletionRequestRequest],
) (*connect.Response[profilesv1.GetDeletionRequestResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	requestID := req.Msg.GetRequestId()
	dr, err := s.service.GetDeletionRequest(ctx, principal.Project.ID, requestID)
	if err != nil {
		if errors.Is(err, coreprofiles.ErrDeletionRequestNotFound) {
			return nil, apperr.NotFound(apperr.ReasonDeletionRequestNotFound, "deletion request not found", apperr.Resource("deletion_request", requestID))
		}
		// Already logged + recorded at the service layer; only translate for the client.
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to get deletion request"))
	}

	return connect.NewResponse(toDeletionRequestResponse(dr)), nil
}

// requestedBy returns the initiating customer id for accountability, or "" for
// API-key callers (Principal.Customer is nil on the API-key auth path).
func requestedBy(p *rpc.Principal) string {
	if p.Customer != nil {
		return p.Customer.ID
	}
	return ""
}

// complianceStatusToProto maps the service's ComplianceStatus to the wire enum.
func complianceStatusToProto(s coreprofiles.ComplianceStatus) profilesv1.ComplianceRequestStatus {
	switch s {
	case coreprofiles.ComplianceStatusPending:
		return profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_PENDING
	case coreprofiles.ComplianceStatusProcessing:
		return profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_PROCESSING
	case coreprofiles.ComplianceStatusCompleted:
		return profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_COMPLETED
	case coreprofiles.ComplianceStatusFailed:
		return profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_FAILED
	default:
		return profilesv1.ComplianceRequestStatus_COMPLIANCE_REQUEST_STATUS_UNSPECIFIED
	}
}

func toDeletionRequestResponse(dr dbread.ComplianceRequest) *profilesv1.GetDeletionRequestResponse {
	resp := &profilesv1.GetDeletionRequestResponse{
		RequestId:        proto.String(dr.ID),
		Status:           complianceStatusToProto(coreprofiles.ComplianceStatus(dr.Status)).Enum(),
		EventsIdentified: proto.Int64(dr.EventsAffected),
		RequestedAt:      postgres.TimestamptzToTimestamp(dr.RequestedAt),
	}
	if dr.ExternalID.Valid {
		resp.ExternalId = proto.String(dr.ExternalID.String)
	}
	if dr.ProfileID.Valid {
		resp.ProfileId = proto.String(dr.ProfileID.String)
	}
	if dr.CompletedAt.Valid {
		resp.CompletedAt = postgres.TimestamptzToTimestamp(dr.CompletedAt)
	}
	if dr.Error.Valid {
		resp.Error = proto.String(dr.Error.String)
	}
	return resp
}

func (s *Server) Get(
	ctx context.Context,
	req *connect.Request[profilesv1.GetRequest],
) (*connect.Response[profilesv1.GetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	pbProfile, err := s.getProfile(ctx, principal.Project.ID, req.Msg.GetId())
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.GetResponse{
		Profile: pbProfile,
	}), nil
}

func (s *Server) GetByExternalId(
	ctx context.Context,
	req *connect.Request[profilesv1.GetByExternalIdRequest],
) (*connect.Response[profilesv1.GetByExternalIdResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, err
	}

	pbProfile, err := s.getProfileByExternalID(ctx, principal.Project.ID, req.Msg.GetExternalId())
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&profilesv1.GetByExternalIdResponse{
		Profile: pbProfile,
	}), nil
}

// pageSize is the server-controlled batch size for streaming profile pages.
// Not configurable by the client — there is no page_size field in the proto request.
const pageSize = 100

func (s *Server) List(
	ctx context.Context,
	req *connect.Request[profilesv1.ListRequest],
	stream *connect.ServerStream[profilesv1.ListResponse],
) error {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return err
	}

	var cursorTime pgtype.Timestamptz
	var cursorID string
	hasCursor := false

	if token := req.Msg.GetPageToken(); token != "" {
		cursor, err := decodeProfileListCursor(token)
		if err != nil {
			return apperr.Invalid(apperr.ReasonInvalidPageToken, "invalid page token")
		}
		cursorTime = postgres.NewTimestamptz(cursor.CreateTime)
		cursorID = cursor.ID
		hasCursor = true
	}

	chFilterCond, err := buildProfileFilterCondition(req.Msg.GetFilterGroups(), req.Msg.GetFilterGroupsOperator())
	if err != nil {
		return apperr.Invalid(apperr.ReasonInvalidProfileFilter, "invalid filters")
	}
	if s.service == nil {
		return connect.NewError(connect.CodeInternal, errors.New("profiles list is unavailable"))
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		chProfiles, err := s.service.List(ctx, coreprofiles.ListParams{
			ProjectID:  principal.Project.ID,
			HasCursor:  hasCursor,
			CursorTime: cursorTime.Time,
			CursorID:   cursorID,
			PageSize:   pageSize + 1,
			Filter:     chFilterCond,
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.ErrorContext(ctx, "failed listing profiles",
				slogx.Error(err),
				slog.String("project_id", principal.Project.ID),
				slog.Bool("has_cursor", hasCursor),
				slog.String("cursor_id", cursorID),
				slog.Time("cursor_time", cursorTime.Time))
			telemetry.RecordError(ctx, err)
			return connect.NewError(connect.CodeInternal, errors.New("failed to list profiles"))
		}

		page, err := buildProfilePage(ctx, chProfiles)
		if err != nil {
			return err
		}
		if page.isEmpty() {
			break
		}
		cursorTime = page.cursorTime
		cursorID = page.cursorID
		hasCursor = true

		nextPageToken := ""
		if page.hasNextPage {
			cursor := &profileListCursor{CreateTime: cursorTime.Time, ID: cursorID}
			nextPageToken, err = cursor.encode()
			if err != nil {
				slog.ErrorContext(ctx, "failed encoding page token", slogx.Error(err))
				telemetry.RecordError(ctx, err)
				return connect.NewError(connect.CodeInternal, errors.New("failed to list profiles"))
			}
		}

		listResp := &profilesv1.ListResponse{
			Profiles: page.profiles,
		}
		if nextPageToken != "" {
			listResp.NextPageToken = proto.String(nextPageToken)
		}
		if err := stream.Send(listResp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.ErrorContext(ctx, "failed sending profile stream", slogx.Error(err),
				slog.String("project_id", principal.Project.ID))
			telemetry.RecordError(ctx, err)
			return connect.NewError(connect.CodeInternal, errors.New("failed to stream profiles"))
		}

		if !page.hasNextPage {
			break
		}
	}

	return nil
}

type profilePage struct {
	profiles    []*profilesv1.Profile
	hasNextPage bool
	cursorTime  pgtype.Timestamptz
	cursorID    string
}

func (p profilePage) isEmpty() bool {
	return len(p.profiles) == 0 && p.cursorID == ""
}

func buildProfilePage(ctx context.Context, profiles []coreprofiles.Profile) (profilePage, error) {
	if len(profiles) == 0 {
		return profilePage{}, nil
	}

	hasNextPage := len(profiles) > pageSize
	pageProfiles := profiles
	if hasNextPage {
		pageProfiles = profiles[:pageSize]
	}

	out := make([]*profilesv1.Profile, 0, len(pageProfiles))
	for _, p := range pageProfiles {
		pbProfile, err := convertProfile(p)
		if err != nil {
			slog.ErrorContext(ctx, "failed converting profile", slogx.Error(err), slog.String("profile_id", p.ID))
			telemetry.RecordError(ctx, err)
			return profilePage{}, connect.NewError(connect.CodeInternal, errors.New("failed to convert profile data"))
		}
		out = append(out, pbProfile)
	}

	last := pageProfiles[len(pageProfiles)-1]
	return profilePage{
		profiles:    out,
		hasNextPage: hasNextPage,
		cursorTime:  postgres.NewTimestamptz(last.CreateTime),
		cursorID:    last.ID,
	}, nil
}

func (s *Server) getProfile(ctx context.Context, projectID, id string) (*profilesv1.Profile, error) {
	if s.service == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("profiles read is unavailable"))
	}
	profile, err := s.service.GetByID(ctx, projectID, id)
	if err != nil {
		if errors.Is(err, coreprofiles.ErrProfileNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProfileNotFound, "profile not found", apperr.Resource("profile", id))
		}
		slog.ErrorContext(ctx, "failed reading profile from clickhouse", slogx.Error(err), slog.String("profile_id", id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to get profile"))
	}
	pbProfile, err := convertProfile(profile)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile", slogx.Error(err), slog.String("profile_id", id))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to convert profile data"))
	}
	return pbProfile, nil
}

func (s *Server) getProfileByExternalID(ctx context.Context, projectID, externalID string) (*profilesv1.Profile, error) {
	if s.service == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("profiles read is unavailable"))
	}
	profile, err := s.service.GetByExternalID(ctx, projectID, externalID)
	if err != nil {
		if errors.Is(err, coreprofiles.ErrProfileNotFound) {
			return nil, apperr.NotFound(apperr.ReasonProfileNotFound, "profile not found", apperr.Resource("profile", externalID))
		}
		slog.ErrorContext(ctx, "failed reading profile by external ID from clickhouse", slogx.Error(err), slog.String("external_id", externalID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to get profile"))
	}
	pbProfile, err := convertProfile(profile)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting profile", slogx.Error(err), slog.String("external_id", externalID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to convert profile data"))
	}
	return pbProfile, nil
}

func convertProfile(p coreprofiles.Profile) (*profilesv1.Profile, error) {
	propertiesMap := p.Properties
	if propertiesMap == nil {
		propertiesMap = make(map[string]any)
	}
	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		return nil, err
	}

	return &profilesv1.Profile{
		CreateTime: timestamppb.New(p.CreateTime),
		ExternalId: proto.String(p.ExternalID),
		Id:         proto.String(p.ID),
		Properties: properties,
		ProjectId:  proto.String(p.ProjectID),
		UpdateTime: timestamppb.New(p.UpdateTime),
		Activity:   convertActivitySummary(p.Activity),
	}, nil
}

func convertActivitySummary(a *coreprofiles.ProfileActivitySummary) *profilesv1.ProfileActivitySummary {
	if a == nil {
		return nil
	}
	return &profilesv1.ProfileActivitySummary{
		FirstSeen:      timestamppb.New(a.FirstSeen),
		LastSeen:       timestamppb.New(a.LastSeen),
		TotalEvents:    proto.Int64(a.TotalEvents),
		Pageviews:      proto.Int64(a.Pageviews),
		Sessions:       proto.Int64(a.Sessions),
		Browser:        proto.String(a.Browser),
		BrowserVersion: proto.String(a.BrowserVersion),
		Os:             proto.String(a.OS),
		OsVersion:      proto.String(a.OSVersion),
		Device:         proto.String(a.Device),
		Country:        proto.String(a.Country),
		Region:         proto.String(a.Region),
		City:           proto.String(a.City),
	}
}
