package jsonutil

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

const confinedExportPayload = "exported transcript"

func TestWriteFileAtomicStreamIn_PinsParentDuringExport(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == testGOOSWindows {
		t.Skip("Windows does not allow renaming an open directory")
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	require.NoError(t, root.Mkdir("tmp", 0o700))
	require.NoError(t, root.WriteFile("tmp/out.json", []byte("old"), 0o600))
	outside := t.TempDir()
	outsideFile := seedAtomicDestination(t, outside)

	err = WriteFileAtomicStreamIn(context.Background(), root, "tmp/out.json", 0o600,
		func(w io.Writer) error {
			// exec.Cmd must receive the file itself to inherit stdout directly.
			_, isFile := w.(*os.File)
			require.True(t, isFile)
			require.NoError(t, root.Rename("tmp", "moved"))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, "tmp")))
			_, err := io.WriteString(w, confinedExportPayload)
			return err
		},
		func(r io.Reader) error {
			data, err := io.ReadAll(r)
			require.NoError(t, err)
			require.Equal(t, confinedExportPayload, string(data))
			return nil
		})
	require.NoError(t, err)
	data, err := root.ReadFile("moved/out.json")
	require.NoError(t, err)
	require.Equal(t, confinedExportPayload, string(data))
	assertNoAtomicTemps(t, filepath.Join(dir, "moved"))
	assertAtomicDestinationUnchanged(t, outsideFile)
	assertNoAtomicTemps(t, outside)
}

func TestWriteFileAtomicStreamIn_RejectsUnsafeParents(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../out.json", "./out.json", "linked/out.json"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			if name == "linked/out.json" {
				require.NoError(t, root.Mkdir("real", 0o700))
				if err := os.Symlink("real", filepath.Join(dir, "linked")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			err = WriteFileAtomicStreamIn(context.Background(), root, name, 0o600,
				func(io.Writer) error {
					t.Fatal("producer ran for an unsafe parent")
					return nil
				}, func(io.Reader) error { return nil })
			require.Error(t, err)
			if name == "linked/out.json" {
				require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
			}
		})
	}
}

func TestWriteFileAtomicStreamIn_FailureOwnership(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"produce", "validate", "publish"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			require.NoError(t, root.Mkdir("tmp", 0o700))
			target := filepath.Join(dir, "tmp", "out.json")
			if failure == "publish" {
				require.NoError(t, root.Mkdir("tmp/out.json", 0o700))
				require.NoError(t, root.WriteFile("tmp/out.json/keep", []byte("old"), 0o600))
			} else {
				seedAtomicDestination(t, filepath.Join(dir, "tmp"))
			}
			wantErr := errors.New("export failed")
			err = WriteFileAtomicStreamIn(context.Background(), root, "tmp/out.json", 0o600,
				func(w io.Writer) error {
					_, err := io.WriteString(w, confinedExportPayload)
					require.NoError(t, err)
					if failure == "produce" {
						return wantErr
					}
					return nil
				}, func(io.Reader) error {
					if failure == "validate" {
						return wantErr
					}
					return nil
				})
			if failure != "publish" {
				require.Same(t, wantErr, err)
				assertAtomicDestinationUnchanged(t, target)
				assertNoAtomicTemps(t, filepath.Join(dir, "tmp"))
				return
			}
			var publishErr *PublishError
			require.ErrorAs(t, err, &publishErr)
			require.Equal(t, filepath.Dir(target), filepath.Dir(publishErr.StagedPath))
			require.True(t, IsTempName(filepath.Base(publishErr.StagedPath)))
			data, readErr := os.ReadFile(publishErr.StagedPath)
			require.NoError(t, readErr)
			require.Equal(t, confinedExportPayload, string(data))
			data, readErr = root.ReadFile("tmp/out.json/keep")
			require.NoError(t, readErr)
			require.Equal(t, "old", string(data))
		})
	}
}
