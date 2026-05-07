package profiles

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/server/rpc"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	workerprofilesv1 "github.com/pug-sh/pug/internal/gen/proto/workers/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	pgW      *pgxpool.Pool
	read     *dbread.Queries
	write    *dbwrite.Queries
	producer *natsdeps.NATSClient
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool, nats *natsdeps.NATSClient) *Server {
	if nats == nil {
		panic("profiles: nats is nil")
	}
	return &Server{
		pgW:      pgW,
		read:     dbread.New(pgRO),
		write:    dbwrite.New(pgW),
		producer: nats,
	}
}

func (s *Server) Delete(
	ctx context.Context,
	req *connect.Request[profilesv1.DeleteRequest],
) (*connect.Response[profilesv1.DeleteResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	// Soft-delete profile and deactivate its devices in a single transaction
	// so devices can't remain active for a deleted profile.
	profileID := req.Msg.GetId()
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed starting delete transaction", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed rolling back delete transaction", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
			telemetry.RecordError(ctx, err)
		}
	}()

	qtx := s.write.WithTx(tx)

	n, err := qtx.SoftDeleteProfileByIDAndProjectID(ctx, dbwrite.SoftDeleteProfileByIDAndProjectIDParams{
		ID:        profileID,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed soft-deleting profile", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
	}

	deactivated, err := qtx.DeactivateDevicesByProfileID(ctx, dbwrite.DeactivateDevicesByProfileIDParams{
		ProfileID: postgres.NewText(profileID),
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed deactivating devices for deleted profile", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}
	slog.InfoContext(ctx, "deactivated devices for deleted profile",
		slog.Int64("count", deactivated),
		slog.String("profile_id", profileID),
		slog.String("project_id", principal.Project.ID))

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "failed committing delete transaction", slogx.Error(err), slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}

	// Best-effort publish to sync deletion to ClickHouse. The PG transaction is
	// already committed, so we return success regardless — a failed NATS publish
	// is logged for reconciliation but must not fail the client request.
	now := timestamppb.New(time.Now())
	upsertMsg := &workerprofilesv1.ProfileUpsertMessage{
		ProfileId:  proto.String(profileID),
		ProjectId:  proto.String(principal.Project.ID),
		IsDeleted:  proto.Bool(true),
		UpdateTime: now,
	}
	upsertData, err := proto.Marshal(upsertMsg)
	if err != nil {
		slog.ErrorContext(ctx, "failed marshalling profile delete upsert message", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
	} else if err = s.producer.Publish(ctx, natsdeps.ProfileUpsertSubject, upsertData); err != nil {
		slog.ErrorContext(ctx, "failed publishing profile delete to NATS", slogx.Error(err),
			slog.String("profile_id", profileID), slog.String("project_id", principal.Project.ID))
		telemetry.RecordError(ctx, err)
	}

	return connect.NewResponse(&profilesv1.DeleteResponse{}), nil
}

func (s *Server) Get(
	ctx context.Context,
	req *connect.Request[profilesv1.GetRequest],
) (*connect.Response[profilesv1.GetResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	p, err := s.read.GetProfileByIDAndProjectID(ctx, dbread.GetProfileByIDAndProjectIDParams{
		ID:        req.Msg.GetId(),
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
		}
		slog.ErrorContext(ctx, "failed reading profile", slogx.Error(err), slog.String("profile_id", req.Msg.GetId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to get profile"))
	}

	pbProfile, err := convertProfile(ctx, p)
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
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	p, err := s.read.GetProfileByProjectAndExternalID(ctx, dbread.GetProfileByProjectAndExternalIDParams{
		ExternalID: req.Msg.GetExternalId(),
		ProjectID:  principal.Project.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
		}
		slog.ErrorContext(ctx, "failed reading profile by external ID", slogx.Error(err), slog.String("external_id", req.Msg.GetExternalId()))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to get profile"))
	}

	pbProfile, err := convertProfile(ctx, p)
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
		return connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	var cursorTime pgtype.Timestamptz
	var cursorID string
	hasCursor := false

	if token := req.Msg.GetPageToken(); token != "" {
		cursor, err := decodeProfileListCursor(token)
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
		cursorTime = postgres.NewTimestamptz(cursor.CreateTime)
		cursorID = cursor.ID
		hasCursor = true
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		profilesList, err := s.read.GetProfilesByProjectID(ctx, dbread.GetProfilesByProjectIDParams{
			ProjectID:  principal.Project.ID,
			HasCursor:  hasCursor,
			CursorTime: cursorTime,
			CursorID:   cursorID,
			PageSize:   pageSize + 1,
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

		if len(profilesList) == 0 {
			break
		}

		hasNextPage := len(profilesList) > pageSize
		pageProfiles := profilesList
		if hasNextPage {
			pageProfiles = profilesList[:pageSize]
		}

		pbProfiles := make([]*profilesv1.Profile, 0, len(pageProfiles))
		for _, p := range pageProfiles {
			pbProfile, err := convertProfile(ctx, p)
			if err != nil {
				return err
			}
			pbProfiles = append(pbProfiles, pbProfile)
		}

		last := pageProfiles[len(pageProfiles)-1]
		cursorTime = last.CreateTime
		cursorID = last.ID
		hasCursor = true

		nextPageToken := ""
		if hasNextPage {
			cursor := &profileListCursor{CreateTime: last.CreateTime.Time, ID: last.ID}
			nextPageToken, err = cursor.encode()
			if err != nil {
				slog.ErrorContext(ctx, "failed encoding page token", slogx.Error(err))
				telemetry.RecordError(ctx, err)
				return connect.NewError(connect.CodeInternal, errors.New("failed to list profiles"))
			}
		}

		listResp := &profilesv1.ListResponse{
			Profiles: pbProfiles,
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

		if !hasNextPage {
			break
		}
	}

	return nil
}

func convertProfile(ctx context.Context, p dbread.Profile) (*profilesv1.Profile, error) {
	propertiesMap := p.Properties
	if propertiesMap == nil {
		propertiesMap = make(map[string]any)
	}
	properties, err := structpb.NewStruct(propertiesMap)
	if err != nil {
		slog.ErrorContext(ctx, "failed converting properties to protobuf struct",
			slogx.Error(err), slog.String("profile_id", p.ID))
		telemetry.RecordError(ctx, err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to convert profile data"))
	}

	return &profilesv1.Profile{
		CreateTime: timestamppb.New(p.CreateTime.Time),
		ExternalId: proto.String(p.ExternalID.String),
		Id:         proto.String(p.ID),
		Properties: properties,
		ProjectId:  proto.String(p.ProjectID),
		UpdateTime: timestamppb.New(p.UpdateTime.Time),
	}, nil
}
