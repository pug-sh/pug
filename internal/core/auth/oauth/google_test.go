package oauth

import (
	"errors"
	"testing"

	"golang.org/x/oauth2"
)

func TestClassifyExchangeError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "invalid_grant",
			err:  &oauth2.RetrieveError{ErrorCode: "invalid_grant"},
			want: ErrOAuthExchangeInvalid,
		},
		{
			name: "server_error",
			err:  &oauth2.RetrieveError{ErrorCode: "server_error"},
			want: ErrOAuthExchangeFailed,
		},
		{
			name: "network",
			err:  errors.New("connection reset"),
			want: ErrOAuthExchangeFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyExchangeError(tc.err); !errors.Is(got, tc.want) {
				t.Fatalf("classifyExchangeError() = %v, want %v", got, tc.want)
			}
		})
	}
}
