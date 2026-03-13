package projects

import (
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

// roToRPCMsg intentionally omits PrivateApiKey — it is only exposed once at creation time via wToRPCMsg.
func roToRPCMsg(p dbread.Project) *projectsv1.Project {
	return &projectsv1.Project{
		CustomerId:     p.CustomerID,
		DisplayName:    p.DisplayName,
		FcmServiceJson: p.FcmServiceJson.String,
		Id:             p.ID,
		PublicApiKey:   p.PublicApiKey,
	}
}

func wToRPCMsg(p dbwrite.Project) *projectsv1.Project {
	return &projectsv1.Project{
		CustomerId:     p.CustomerID,
		DisplayName:    p.DisplayName,
		FcmServiceJson: p.FcmServiceJson.String,
		Id:             p.ID,
		PrivateApiKey:  p.PrivateApiKey,
		PublicApiKey:   p.PublicApiKey,
	}
}
