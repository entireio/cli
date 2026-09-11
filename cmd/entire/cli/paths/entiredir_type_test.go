package paths

import (
	"io/fs"
	"testing"
)

// The entries directly under `.entire` are Entire's own, and Entire only ever
// creates regular files and directories there. Anything else arrived some other
// way, so the check is an allowlist rather than a list of known-bad types: a
// mode bit nobody has thought about yet is refused by default.
//
// ModeIrregular is the deliberate exception, and the reason it cannot be part
// of the allowlist is that Windows overloads it. Go maps every reparse tag it
// has no category for onto that single bit, which puts NTFS directory junctions
// and OneDrive Files On-Demand placeholders in the same bucket. Refusing the
// bucket would hard-fail every command in a repo inside a synced folder, and
// the placeholder gets there without anyone attacking anything, while a
// junction cannot arrive by checkout at all: git has no tree-object mode for
// one. Tolerating the ambiguous bit is the cheaper mistake.
//
// It is tolerated by being masked out rather than matched on, because Windows
// hands it over alongside ModeDir whenever the reparse tag is not a name
// surrogate, which is what a placeholder directory is. Rejected types are
// rejected regardless of the bit.
func TestUnsupportedEntryType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode fs.FileMode
		want bool
	}{
		{name: "regular file", mode: 0},
		{name: "directory", mode: fs.ModeDir},
		{name: "symbolic link", mode: fs.ModeSymlink, want: true},
		{name: "named pipe", mode: fs.ModeNamedPipe, want: true},
		{name: "socket", mode: fs.ModeSocket, want: true},
		{name: "block device", mode: fs.ModeDevice, want: true},
		{name: "character device", mode: fs.ModeDevice | fs.ModeCharDevice, want: true},
		{
			// A junction, or a cloud placeholder standing in for a file.
			name: "irregular is tolerated",
			mode: fs.ModeIrregular,
		},
		{
			// A cloud placeholder standing in for a directory. Go withholds
			// ModeDir only for a name-surrogate reparse tag, and the cloud tags
			// are not surrogates, so this is the shape `.entire/metadata`,
			// `logs`, and `tmp` take inside a synced folder. Demanding
			// ModeIrregular stand alone would reject the very case it is
			// tolerated for.
			name: "irregular alongside a directory is tolerated",
			mode: fs.ModeDir | fs.ModeIrregular,
		},
		{
			// Nothing produces this today, and if something starts to, the
			// symlink half is the half that matters.
			name: "irregular alongside a rejected bit is still rejected",
			mode: fs.ModeIrregular | fs.ModeSymlink,
			want: true,
		},
		{
			name: "irregular alongside a directory and a rejected bit is still rejected",
			mode: fs.ModeDir | fs.ModeIrregular | fs.ModeNamedPipe,
			want: true,
		},
		{
			// The allowlist is exclusive, so the directory bit does not
			// launder a rejected one. fs.FileMode.IsDir tests a single bit,
			// which is why this must be checked against the whole type rather
			// than against IsDir.
			name: "directory alongside a rejected bit is still rejected",
			mode: fs.ModeDir | fs.ModeSymlink,
			want: true,
		},
		{
			name: "directory alongside a named pipe is still rejected",
			mode: fs.ModeDir | fs.ModeNamedPipe,
			want: true,
		},
		{
			// What os.ReadDir's direntType returns when the platform reports
			// no type and the internal Lstat cannot supply one. Every bit is
			// set, the directory bit among them, so an allowlist keyed on
			// IsDir would wave it through.
			name: "unknown type is rejected",
			mode: ^fs.FileMode(0),
			want: true,
		},
		{
			// Permission and non-type bits do not change what an entry is.
			name: "regular file with permission bits",
			mode: 0o644,
		},
		{
			name: "irregular with permission bits is tolerated",
			mode: fs.ModeIrregular | 0o644,
		},
		{
			name: "setuid regular file",
			mode: fs.ModeSetuid | 0o755,
		},
		{
			name: "directory with permission bits",
			mode: fs.ModeDir | 0o755,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := unsupportedEntryType(tt.mode); got != tt.want {
				t.Errorf("unsupportedEntryType(%v) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
