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

func (s *Server) List(
	ctx context.Context,
	_ *connect.Request[profilesv1.ListRequest],
) (*connect.Response[profilesv1.ListResponse], error) {
	principal, err := rpc.MustGetPrincipalWithProject(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}

	profilesList, err := s.read.GetProfilesByProjectID(ctx, principal.Project.ID)
	if err != nil {
		slog.ErrorContext(ctx, "failed listing profiles", slogx.Error(err), slog.String("projectId", principal.Project.ID))
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to list profiles"))
	}

	pbProfiles := make([]*profilesv1.Profile, len(profilesList))
	for i, p := range profilesList {
		pbProfile, err := convertProfile(ctx, p)
		if err != nil {
			return nil, err
		}
		pbProfiles[i] = pbProfile
	}

	return connect.NewResponse(&profilesv1.ListResponse{
		Profiles: pbProfiles,
	}), nil
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
