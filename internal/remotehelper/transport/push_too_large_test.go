package transport

import (
	"errors"
	"testing"
)

func TestPushTooLargeError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		msg       string
		wantText  string
		wantSize  int64
		wantLimit int64
	}{
		{
			name:      "declared",
			msg:       "push rejected: declared size 3221225472 exceeds limit 2147483648",
			wantText:  "entire: push too large: size 3221225472 exceeds limit 2147483648",
			wantSize:  3221225472,
			wantLimit: 2147483648,
		},
		{
			name:     "unparsable",
			msg:      "too big",
			wantText: "entire: push too large: too big",
		},
		{
			name:     "empty body",
			msg:      "",
			wantText: "entire: push too large: the server refused the push body as too large",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error = newPushTooLargeError(tc.msg)
			var tooLarge *PushTooLargeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("want *PushTooLargeError, got %T: %v", err, err)
			}
			if err.Error() != tc.wantText {
				t.Errorf("Error() = %q, want %q", err.Error(), tc.wantText)
			}
			if tooLarge.Size != tc.wantSize || tooLarge.Limit != tc.wantLimit {
				t.Errorf("size/limit = %d/%d, want %d/%d", tooLarge.Size, tooLarge.Limit, tc.wantSize, tc.wantLimit)
			}
		})
	}
}
