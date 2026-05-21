package apperr

import "google.golang.org/genproto/googleapis/rpc/errdetails"

// Resource attaches a google.rpc.ResourceInfo (which resource was missing/duplicated).
// Each call appends a distinct ResourceInfo — it is intentionally not coalesced,
// since two calls describe two different resources.
func Resource(resourceType, name string) Option {
	return func(s *detailSink) {
		s.details = append(s.details, &errdetails.ResourceInfo{
			ResourceType: resourceType,
			ResourceName: name,
		})
	}
}

// Precondition appends a violation to a single google.rpc.PreconditionFailure
// (reusing one if already present so multiple options coalesce).
func Precondition(typ, subject, description string) Option {
	return func(s *detailSink) {
		v := &errdetails.PreconditionFailure_Violation{Type: typ, Subject: subject, Description: description}
		for _, d := range s.details {
			if pf, ok := d.(*errdetails.PreconditionFailure); ok {
				pf.Violations = append(pf.Violations, v)
				return
			}
		}
		s.details = append(s.details, &errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{v},
		})
	}
}

// Field appends a field violation to a single google.rpc.BadRequest.
func Field(field, description string) Option {
	return func(s *detailSink) {
		fv := &errdetails.BadRequest_FieldViolation{Field: field, Description: description}
		for _, d := range s.details {
			if br, ok := d.(*errdetails.BadRequest); ok {
				br.FieldViolations = append(br.FieldViolations, fv)
				return
			}
		}
		s.details = append(s.details, &errdetails.BadRequest{
			FieldViolations: []*errdetails.BadRequest_FieldViolation{fv},
		})
	}
}
