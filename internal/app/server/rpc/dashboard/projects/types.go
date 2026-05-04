package projects

import (
	"google.golang.org/protobuf/proto"

	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// roToRPCMsg and wToRPCMsg must be kept in sync — they convert
// the read and write models to the same proto message.
// roToRPCMsg intentionally omits PrivateApiKey — it is only exposed once at creation time via wToRPCMsgWithPrivateKey.
func roToRPCMsg(p dbread.Project) *projectsv1.Project {
	return &projectsv1.Project{
		DisplayName:    proto.String(p.DisplayName),
		FcmServiceJson: proto.String(p.FcmServiceJson.String),
		Id:             proto.String(p.ID),
		OrgId:          proto.String(p.OrgID),
		PublicApiKey:   proto.String(p.PublicApiKey),
	}
}

// wToRPCMsg converts a write model to RPC message without the private key.
func wToRPCMsg(p dbwrite.Project) *projectsv1.Project {
	return &projectsv1.Project{
		DisplayName:    proto.String(p.DisplayName),
		FcmServiceJson: proto.String(p.FcmServiceJson.String),
		Id:             proto.String(p.ID),
		OrgId:          proto.String(p.OrgID),
		PublicApiKey:   proto.String(p.PublicApiKey),
	}
}

// wToRPCMsgWithPrivateKey includes the private key — use only for Create responses.
func wToRPCMsgWithPrivateKey(p dbwrite.Project) *projectsv1.Project {
	msg := wToRPCMsg(p)
	msg.PrivateApiKey = proto.String(p.PrivateApiKey)
	return msg
}
