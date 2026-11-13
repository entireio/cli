//go:build e2e

package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/e2e/agents"
	"github.com/entireio/cli/e2e/entire"
	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/require"
)

// A 16x16 lossless WebP filled with #ffd700. The standard library has no WebP
// encoder, so this one is stored.
const sampleWebP = "UklGRhwAAABXRUJQVlA4TBAAAAAvD8ADAAfQ6/5H/wMR0f8A"

type sampleImage struct {
	path      string
	mediaType string
	data      []byte
}

// TestResumeRestoredSessionWithDroppedImages checks that a session whose
// checkpoint was written with redaction.drop_images still resumes in its
// agent: the restored transcript holds a 1x1 placeholder for every image, and
// the next turn replays that history to the provider, which must accept it.
func TestResumeRestoredSessionWithDroppedImages(t *testing.T) {
	agentNames := []string{"claude-code", "codex", "pi", "opencode"}
	testutil.ForEachNamedAgent(t, 6*time.Minute, agentNames, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		var opts []agents.Option
		var homeEnv []string
		if homed, ok := s.Agent.(agents.HomedAgent); ok {
			home, err := homed.NewHome(s.Dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			opts = append(opts, agents.WithHome(home))
			homeEnv = homed.HomeEnv(home)
		}

		testutil.PatchSettings(t, s.Dir, map[string]any{
			"redaction": map[string]any{"drop_images": true},
		})
		images := writeSampleImages(t, s.Dir)

		mainBranch := testutil.GitOutput(t, s.Dir, "branch", "--show-current")
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add sample images")
		s.Git(t, "checkout", "-b", "feature")

		paths := make([]string, 0, len(images))
		for _, img := range images {
			paths = append(paths, img.path)
		}
		out, err := s.RunPrompt(t, ctx,
			"Open each of these image files with your file-reading tool so you can see it: "+
				strings.Join(paths, ", ")+
				". Then create colors.md with one line per file naming its color. Do not commit. Do not ask for confirmation.",
			opts...)
		require.NoError(t, err, "first prompt failed\nstdout: %s\nstderr: %s", out.Stdout, out.Stderr)
		testutil.AssertFileExists(t, s.Dir, "colors.md")

		s.Git(t, "add", "colors.md")
		s.Git(t, "commit", "-m", "Add colors")
		testutil.WaitForSessionIdle(t, s.Dir, 15*time.Second)
		testutil.WaitForCheckpoint(t, s, 30*time.Second)

		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		assertImagesDropped(t, s, testutil.ReadCheckpointTranscript(t, s.Dir, checkpointID, 0), images)

		s.Git(t, "checkout", mainBranch)
		resumeOut, err := entire.ResumeWithEnv(s.Dir, "feature", homeEnv)
		require.NoError(t, err, "entire resume failed: %s", resumeOut)
		sessionID := resumedSessionID(t, s.Agent.Name(), resumeOut)

		out, err = s.RunPrompt(t, ctx,
			"Reply with exactly the word RESUMED and nothing else. Do not use any tools.",
			append(opts, agents.WithResume(sessionID))...)
		require.NoError(t, err, "resumed prompt failed\nstdout: %s\nstderr: %s", out.Stdout, out.Stderr)
		require.Contains(t, out.Stdout, "RESUMED", "resumed session should answer\nstderr: %s", out.Stderr)
	})
}

// writeSampleImages writes one solid 16x16 image of each type the placeholders
// cover, each a different color so the agent has something to describe.
func writeSampleImages(t *testing.T, dir string) []sampleImage {
	t.Helper()

	solid := func(c color.Color) image.Image {
		img := image.NewRGBA(image.Rect(0, 0, 16, 16))
		for y := range 16 {
			for x := range 16 {
				img.Set(x, y, c)
			}
		}
		return img
	}
	encode := func(enc func(*bytes.Buffer) error) []byte {
		var buf bytes.Buffer
		require.NoError(t, enc(&buf))
		return buf.Bytes()
	}
	webp, err := base64.StdEncoding.DecodeString(sampleWebP)
	require.NoError(t, err)

	images := []sampleImage{
		{"images/red.png", "image/png", encode(func(b *bytes.Buffer) error {
			return png.Encode(b, solid(color.RGBA{R: 255, A: 255}))
		})},
		{"images/green.jpg", "image/jpeg", encode(func(b *bytes.Buffer) error {
			return jpeg.Encode(b, solid(color.RGBA{G: 255, A: 255}), nil)
		})},
		{"images/blue.gif", "image/gif", encode(func(b *bytes.Buffer) error {
			return gif.Encode(b, solid(color.RGBA{B: 255, A: 255}), nil)
		})},
		{"images/yellow.webp", "image/webp", webp},
	}

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "images"), 0o755))
	for _, img := range images {
		require.NoError(t, os.WriteFile(filepath.Join(dir, img.path), img.data, 0o644))
	}
	return images
}

// assertImagesDropped fails unless the stored transcript holds no sample image
// and at least one placeholder, so a pass means placeholders were replayed.
// Agents may re-encode an image before storing it, so which placeholder types
// appear is logged rather than required.
func assertImagesDropped(t *testing.T, s *testutil.RepoState, transcript string, images []sampleImage) {
	t.Helper()
	require.NotEmpty(t, transcript, "checkpoint transcript should exist")
	t.Cleanup(func() {
		if t.Failed() {
			_ = os.WriteFile(filepath.Join(s.ArtifactDir, "stored-transcript.jsonl"), []byte(transcript), 0o644)
		}
	})

	var present []string
	seen := map[string]bool{}
	for _, img := range images {
		// Not require.NotContains: its message would print the whole transcript.
		if strings.Contains(transcript, base64.StdEncoding.EncodeToString(img.data)) {
			t.Fatalf("stored transcript still holds %s; see stored-transcript.jsonl in the artifacts", img.path)
		}
		ph := placeholderFor(t, img)
		if !seen[ph] && strings.Contains(transcript, ph) {
			present = append(present, img.mediaType)
		}
		seen[ph] = true
	}
	require.NotEmpty(t, present, "stored transcript holds no image placeholder; the agent never stored an image")
	t.Logf("placeholders in stored transcript: %s", strings.Join(present, ", "))
}

// placeholderFor returns the base64 placeholder DropImages stores for img.
func placeholderFor(t *testing.T, img sampleImage) string {
	t.Helper()
	prefix := "data:" + img.mediaType + ";base64,"
	uri := `"` + prefix + base64.StdEncoding.EncodeToString(img.data) + `"`
	dropped := string(imageextract.DropImages([]byte(uri)))
	require.NotEqual(t, uri, dropped, "DropImages left %s unchanged", img.path)
	return strings.TrimSuffix(strings.TrimPrefix(dropped, `"`+prefix), `"`)
}

// resumedSessionID reads the session ID from the resume command `entire
// resume` prints for the agent.
func resumedSessionID(t *testing.T, agentName, resumeOut string) string {
	t.Helper()
	commands := map[string]string{
		"claude-code": `claude -r (\S+)`,
		"codex":       `codex resume (\S+)`,
		"pi":          `pi --session (\S+)`,
		"opencode":    `opencode -s (\S+)`,
	}
	pattern, ok := commands[agentName]
	require.True(t, ok, "no resume command known for %s", agentName)
	m := regexp.MustCompile(pattern).FindStringSubmatch(resumeOut)
	require.NotNil(t, m, "resume output has no %q command:\n%s", pattern, resumeOut)
	return m[1]
}
