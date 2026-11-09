package mcpgrants

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coremcpgrants "github.com/pug-sh/pug/internal/core/mcpgrants"
	mcpgrantsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/mcpgrants/v1"
)

func authorizationRequest(m *mcpgrantsv1.AuthorizationRequest) coremcpgrants.AuthorizationRequest {
	return coremcpgrants.AuthorizationRequest{
		ClientID:            m.GetClientId(),
		CodeChallenge:       m.GetCodeChallenge(),
		CodeChallengeMethod: m.GetCodeChallengeMethod(),
		RedirectURI:         m.GetRedirectUri(),
		Resource:            m.GetResource(),
		ResponseType:        m.GetResponseType(),
		State:               m.GetState(),
	}
}

func projectToRPC(p coremcpgrants.Project) *mcpgrantsv1.Project {
	return &mcpgrantsv1.Project{
		Id:             proto.String(p.ID),
		DisplayName:    proto.String(p.DisplayName),
		OrgId:          proto.String(p.OrgID),
		OrgDisplayName: proto.String(p.OrgDisplayName),
	}
}

func grantToRPC(g coremcpgrants.Grant) *mcpgrantsv1.Grant {
	orgs := make([]*mcpgrantsv1.Org, len(g.Orgs))
	for i, o := range g.Orgs {
		orgs[i] = &mcpgrantsv1.Org{Id: proto.String(o.ID), DisplayName: proto.String(o.DisplayName)}
	}
	return &mcpgrantsv1.Grant{
		Id:           proto.String(g.ID),
		ClientHost:   proto.String(g.ClientHost),
		ClientName:   proto.String(g.ClientName),
		CreateTime:   timestamppb.New(g.CreateTime),
		LastUsedTime: timestamppb.New(g.LastUsedTime),
		Orgs:         orgs,
	}
}
