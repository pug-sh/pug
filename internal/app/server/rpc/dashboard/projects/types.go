package projects

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// roToRPCMsg converts a project to its proto message. It carries no key: a
// project's keys are their own resource, read via ListApiKeys.
func roToRPCMsg(p dbread.Project) *projectsv1.Project {
	return &projectsv1.Project{
		DisplayName:       proto.String(p.DisplayName),
		FcmServiceJson:    proto.String(p.FcmServiceJson.String),
		Id:                proto.String(p.ID),
		OrgId:             proto.String(p.OrgID),
		ReportingTimezone: proto.String(p.ReportingTimezone),
	}
}

// wToRPCMsg converts a write model to RPC message. dbread.Project and
// dbwrite.Project are the same projects row generated twice, so the conversion
// costs nothing and there is one mapper to keep correct rather than two to keep
// in sync — were sqlc ever to make them differ, this stops compiling.
func wToRPCMsg(p dbwrite.Project) *projectsv1.Project {
	return roToRPCMsg(dbread.Project(p))
}

// kindToRPCEnum and kindFromRPCEnum map the stored kind to and from the wire
// enum. Both fail closed on a value they don't know rather than guessing a
// flavor: kindFromRPCEnum's ok=false is unreachable through the RPC (the request
// is protovalidated defined_only + not_in [0]) and means a new enum value shipped
// without a mapping.
func kindToRPCEnum(kind string) projectsv1.ApiKeyKind {
	switch coreprojects.Kind(kind) {
	case coreprojects.KindPublic:
		return projectsv1.ApiKeyKind_API_KEY_KIND_PUBLIC
	case coreprojects.KindPrivate:
		return projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE
	default:
		return projectsv1.ApiKeyKind_API_KEY_KIND_UNSPECIFIED
	}
}

func kindFromRPCEnum(kind projectsv1.ApiKeyKind) (coreprojects.Kind, bool) {
	switch kind {
	case projectsv1.ApiKeyKind_API_KEY_KIND_PUBLIC:
		return coreprojects.KindPublic, true
	case projectsv1.ApiKeyKind_API_KEY_KIND_PRIVATE:
		return coreprojects.KindPrivate, true
	default:
		return "", false
	}
}

// apiKeyToRPCMsg converts a stored key to its proto message. The Key field is
// populated only for a public key: a private key's token is a digest, so there is
// nothing to return but the mask.
func apiKeyToRPCMsg(k dbread.ApiKey) *projectsv1.ApiKey {
	msg := &projectsv1.ApiKey{
		CreateTime:  timestamppb.New(k.CreateTime.Time),
		DisplayName: proto.String(k.DisplayName),
		Id:          proto.String(k.ID),
		Kind:        kindToRPCEnum(k.Kind).Enum(),
		Masked:      proto.String(k.Masked),
	}
	if coreprojects.Kind(k.Kind) == coreprojects.KindPublic {
		msg.Key = proto.String(k.Token)
	}
	return msg
}

// createdApiKeyToRPCMsg maps a freshly created key, reusing the read mapper the
// same way wToRPCMsg does: dbread.ApiKey and dbwrite.ApiKey are the same api_keys
// row generated twice, so the conversion costs nothing — and were sqlc ever to
// make them differ, this stops compiling rather than drifting. The raw *private*
// key rides in CreateApiKeyResponse.Key, never in the message; a public key's
// token is its raw value, so apiKeyToRPCMsg does echo that one back in Key.
func createdApiKeyToRPCMsg(k dbwrite.ApiKey) *projectsv1.ApiKey {
	return apiKeyToRPCMsg(dbread.ApiKey(k))
}
