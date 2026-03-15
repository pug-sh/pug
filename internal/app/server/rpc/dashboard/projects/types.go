package projects

import (
	projectsv1 "github.com/fivebitsio/cotton/internal/gen/proto/projects/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
)

// roToRPCMsg intentionally omits PrivateApiKey — it is only exposed once at creation time via wToRPCMsgWithPrivateKey.
func roToRPCMsg(p dbread.Project) *projectsv1.Project {
	return &projectsv1.Project{
		CustomerId:     p.CustomerID,
		DisplayName:    p.DisplayName,
		FcmServiceJson: p.FcmServiceJson.String,
		Id:             p.ID,
		PublicApiKey:   p.PublicApiKey,
	}
}

// wToRPCMsg converts a write model to RPC message without the private key.
func wToRPCMsg(p dbwrite.Project) *projectsv1.Project {
	return &projectsv1.Project{
		CustomerId:     p.CustomerID,
		DisplayName:    p.DisplayName,
		FcmServiceJson: p.FcmServiceJson.String,
		Id:             p.ID,
		PublicApiKey:   p.PublicApiKey,
	}
}

// wToRPCMsgWithPrivateKey includes the private key — use only for Create responses.
func wToRPCMsgWithPrivateKey(p dbwrite.Project) *projectsv1.Project {
	msg := wToRPCMsg(p)
	msg.PrivateApiKey = p.PrivateApiKey
	return msg
}
