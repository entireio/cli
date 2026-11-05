package userconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  string
		want    Config
		wantErr string
	}{
		{name: "missing file"},
		{name: "empty object", config: `{}`},
		{name: "token store", config: `{"token_store": "file"}`, want: Config{TokenStore: "file"}},
		{name: "no auto update", config: `{"no_auto_update": true}`, want: Config{NoAutoUpdate: true}},
		{name: "no auto update false", config: `{"no_auto_update": false}`},
		{
			name:   "both keys",
			config: `{"token_store": "keyring", "no_auto_update": true}`,
			want:   Config{TokenStore: "keyring", NoAutoUpdate: true},
		},
		{name: "no auto update string", config: `{"no_auto_update": "yes"}`, wantErr: "no_auto_update"},
		{name: "wrong case", config: `{"No_Auto_Update": true}`, wantErr: `unknown key "No_Auto_Update"`},
		{name: "unknown token store", config: `{"token_store": "fiel"}`, wantErr: `token_store is "fiel"`},
		{name: "malformed", config: `{`, wantErr: "unexpected end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", dir)
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := Load()

			if tc.wantErr != "" {
				if !errors.Is(err, ErrBadConfig) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() err = %v, want ErrBadConfig naming %q", err, tc.wantErr)
				}
				for _, want := range []string{filepath.Join(dir, FileName), "fix or remove it"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Load() err = %q, want it to contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() err = %v", err)
			}
			if got != tc.want {
				t.Errorf("Load() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
