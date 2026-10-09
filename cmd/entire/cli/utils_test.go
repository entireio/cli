package cli

import (
	"bytes"
	"errors"
	"testing"

	"charm.land/huh/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleFormCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		action  string
		err     error
		wantOut string
		wantErr bool
	}{
		{
			name:    "user abort prints cancelled and returns nil",
			action:  "Reset",
			err:     huh.ErrUserAborted,
			wantOut: "Reset cancelled.\n",
			wantErr: false,
		},
		{
			name:    "action name used in cancelled message",
			action:  "Trail creation",
			err:     huh.ErrUserAborted,
			wantOut: "Trail creation cancelled.\n",
			wantErr: false,
		},
		{
			name:    "timeout prints cancelled and returns nil",
			action:  "Stop",
			err:     huh.ErrTimeout,
			wantOut: "Stop cancelled.\n",
			wantErr: false,
		},
		{
			name:    "unexpected error is wrapped with action name",
			action:  "Reset",
			err:     errors.New("form exploded"),
			wantOut: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := handleFormCancellation(&out, tt.action, tt.err)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantOut, out.String())
		})
	}
}
