package apperr

import "google.golang.org/genproto/googleapis/rpc/errdetails"

// Resource attaches a google.rpc.ResourceInfo (which resource was missing/duplicated).
func Resource(resourceType, name string) Option {
	return func(e *Error) {
		e.details = append(e.details, &errdetails.ResourceInfo{
			ResourceType: resourceType,
			ResourceName: name,
		})
	}
}

// Precondition appends a violation to a single google.rpc.PreconditionFailure
// (reusing one if already present so multiple options coalesce).
func Precondition(typ, subject, description string) Option {
	v := &errdetails.PreconditionFailure_Violation{Type: typ, Subject: subject, Description: description}
	return func(e *Error) {
		for _, d := range e.details {
			if pf, ok := d.(*errdetails.PreconditionFailure); ok {
				pf.Violations = append(pf.Violations, v)
				return
			}
		}
		e.details = append(e.details, &errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{v},
		})
	}
}

// Field appends a field violation to a single google.rpc.BadRequest.
func Field(field, description string) Option {
	fv := &errdetails.BadRequest_FieldViolation{Field: field, Description: description}
	return func(e *Error) {
		for _, d := range e.details {
			if br, ok := d.(*errdetails.BadRequest); ok {
				br.FieldViolations = append(br.FieldViolations, fv)
				return
			}
		}
		e.details = append(e.details, &errdetails.BadRequest{
			FieldViolations: []*errdetails.BadRequest_FieldViolation{fv},
		})
	}
}
