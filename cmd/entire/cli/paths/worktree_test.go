package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetWorktreeID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupFunc  func(dir string) error
		wantID     string
		wantErr    bool
		errContain string
	}{
		{
			name: "main worktree (git directory)",
			setupFunc: func(dir string) error {
				return os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
			},
			wantID: "",
		},
		{
			name: "ordinary submodule relative gitdir",
			setupFunc: func(dir string) error {
				content := "gitdir: ../../.git/modules/deps/go-git\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "",
		},
		{
			name: "ordinary submodule absolute gitdir",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.git/modules/deps/go-git\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "",
		},
		{
			name: "nested ordinary submodule gitdir",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.git/modules/libs/go-git/modules/vendor/crypto\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "",
		},
		{
			name: "linked worktree of submodule",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.git/modules/deps/go-git/worktrees/sub-linked\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "sub-linked",
		},
		{
			name: "ordinary submodule inside linked superproject worktree",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.git/worktrees/super-linked/modules/deps/go-git\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "",
		},
		{
			name: "linked worktree simple name",
			setupFunc: func(dir string) error {
				content := "gitdir: /some/repo/.git/worktrees/test-wt\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "test-wt",
		},
		{
			name: "linked worktree with subdirectory name",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.git/worktrees/feature/auth-system\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "feature/auth-system",
		},
		{
			name: "no .git exists",
			setupFunc: func(_ string) error {
				return nil // Don't create .git
			},
			wantErr:    true,
			errContain: "failed to stat .git",
		},
		{
			name: "invalid .git file format",
			setupFunc: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, ".git"), []byte("invalid content"), 0o644)
			},
			wantErr:    true,
			errContain: "invalid .git file format",
		},
		{
			name: "bare repo worktree simple name",
			setupFunc: func(dir string) error {
				content := "gitdir: /some/repo/.bare/worktrees/main\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "main",
		},
		{
			name: "bare repo worktree with subdirectory name",
			setupFunc: func(dir string) error {
				content := "gitdir: /repo/.bare/worktrees/feature/login\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantID: "feature/login",
		},
		{
			name: "gitdir without worktrees path",
			setupFunc: func(dir string) error {
				content := "gitdir: /some/repo/.git\n"
				return os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o644)
			},
			wantErr:    true,
			errContain: "unexpected gitdir format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if err := tt.setupFunc(dir); err != nil {
				t.Fatalf("setup failed: %v", err)
			}

			id, err := GetWorktreeID(dir)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetWorktreeID() error = nil, want error containing %q", tt.errContain)
				}
				if tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("GetWorktreeID() error = %v, want error containing %q", err, tt.errContain)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetWorktreeID() error = %v, want nil", err)
			}
			if id != tt.wantID {
				t.Errorf("GetWorktreeID() = %q, want %q", id, tt.wantID)
			}
		})
	}
}
