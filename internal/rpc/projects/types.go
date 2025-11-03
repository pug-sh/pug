package projects

import (
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

func roToRPCMsg(p dbread.Project) *projectsv1.Project {
	return &projectsv1.Project{
		ApiKey:      p.ApiKey,
		CustomerId:  p.CustomerID,
		DisplayName: p.DisplayName,
		Id:          p.ID,
	}
}

func wToRPCMsg(p dbwrite.Project) *projectsv1.Project {
	return &projectsv1.Project{
		ApiKey:      p.ApiKey,
		CustomerId:  p.CustomerID,
		DisplayName: p.DisplayName,
		Id:          p.ID,
	}
}
