package coreapi

import (
	"errors"
	"fmt"
	"testing"
)

func TestAPIError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "non-API error returns empty",
			err:  errors.New("dial tcp: connection refused"),
			want: "",
		},
		{
			name: "prefers detail",
			err: &ErrorModelStatusCode{
				StatusCode: 409,
				Response: ErrorModel{
					Title:  NewOptString("Conflict"),
					Detail: NewOptString("organization name already taken"),
				},
			},
			want: "organization name already taken",
		},
		{
			name: "falls back to title when detail empty",
			err: &ErrorModelStatusCode{
				StatusCode: 403,
				Response:   ErrorModel{Title: NewOptString("Forbidden")},
			},
			want: "Forbidden",
		},
		{
			name: "falls back to status when title and detail empty",
			err:  &ErrorModelStatusCode{StatusCode: 500},
			want: "control-plane request failed with status 500",
		},
		{
			name: "unwraps a wrapped API error",
			err: fmt.Errorf("create org: %w", &ErrorModelStatusCode{
				StatusCode: 422,
				Response:   ErrorModel{Detail: NewOptString("name is required")},
			}),
			want: "name is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := APIError(tc.err); got != tc.want {
				t.Errorf("APIError() = %q, want %q", got, tc.want)
			}
		})
	}
}
