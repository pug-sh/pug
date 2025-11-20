package segments

import (
	"github.com/fivebitsio/cotton/internal/core/segments"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

type Server struct {
	service segments.Repo
}

func NewServer(dbRead *dbread.Queries, dbWrite *dbwrite.Queries) *Server {
	segmentService := segments.NewService(dbWrite, dbRead)
	return &Server{
		service: segmentService,
	}
}

func (s *Server) Service() segments.Repo {
	return s.service
}