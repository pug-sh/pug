package profiles

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	read  *dbread.Queries
	write *dbwrite.Queries
}

func NewServer(pgRO *pgxpool.Pool, pgW *pgxpool.Pool) *Server {
	return &Server{
		read:  dbread.New(pgRO),
		write: dbwrite.New(pgW),
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

	n, err := s.write.DeleteProfileByIDAndProjectID(ctx, dbwrite.DeleteProfileByIDAndProjectIDParams{
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed deleting profile", slogx.Error(err), slog.String("profileId", req.Msg.Id))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to delete profile"))
	}
	if n == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
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
		ID:        req.Msg.Id,
		ProjectID: principal.Project.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
		}
		slog.ErrorContext(ctx, "failed reading profile", slogx.Error(err), slog.String("profileId", req.Msg.Id))
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
		ExternalID: req.Msg.ExternalId,
		ProjectID:  principal.Project.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
		}
		slog.ErrorContext(ctx, "failed reading profile by external ID", slogx.Error(err), slog.String("externalId", req.Msg.ExternalId))
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

	if req.Msg.GetCursor() != nil {
		cursorTime = pgtype.Timestamptz{Time: req.Msg.GetCursor().GetCursorTime().AsTime(), Valid: true}
		cursorID = req.Msg.GetCursor().GetCursorId()
		hasCursor = true
	}

	for {
		profilesList, err := s.read.GetProfilesByProjectID(ctx, dbread.GetProfilesByProjectIDParams{
			ProjectID:  principal.Project.ID,
			HasCursor:  hasCursor,
			CursorTime: cursorTime,
			CursorID:   cursorID,
			PageSize:   pageSize,
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed listing profiles", slogx.Error(err), slog.String("projectId", principal.Project.ID))
			return connect.NewError(connect.CodeInternal, errors.New("failed to list profiles"))
		}

		if len(profilesList) == 0 {
			break
		}

		for _, p := range profilesList {
			pbProfile, err := convertProfile(ctx, p)
			if err != nil {
				return err
			}

			cursorTime = p.CreateTime
			cursorID = p.ID

			if err := stream.Send(&profilesv1.ListResponse{
				Profiles: []*profilesv1.Profile{pbProfile},
				Cursor: &profilesv1.Cursor{
					CursorTime: timestamppb.New(cursorTime.Time),
					CursorId:   cursorID,
				},
			}); err != nil {
				slog.ErrorContext(ctx, "failed sending profile stream", slogx.Error(err))
				return connect.NewError(connect.CodeInternal, errors.New("failed to stream profiles"))
			}
		}

		last := profilesList[len(profilesList)-1]
		cursorTime = last.CreateTime
		cursorID = last.ID
		hasCursor = true

		if len(profilesList) < pageSize {
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
			slogx.Error(err), slog.String("profileId", p.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to convert profile data"))
	}

	return &profilesv1.Profile{
		CreateTime: timestamppb.New(p.CreateTime.Time),
		ExternalId: p.ExternalID.String,
		Id:         p.ID,
		Properties: properties,
		ProjectId:  p.ProjectID,
		UpdateTime: timestamppb.New(p.UpdateTime.Time),
	}, nil
}
