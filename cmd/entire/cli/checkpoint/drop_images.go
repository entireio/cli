package checkpoint

import (
	"context"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
)

// DropImagesIfEnabled replaces every image in a transcript with a 1x1
// placeholder when redaction.drop_images is on, and returns the transcript
// unchanged otherwise. Every path that stores a transcript calls it after
// sanitizing and before redacting: redaction's image skip rules would
// otherwise decide which images survive.
func DropImagesIfEnabled(ctx context.Context, transcript []byte) []byte {
	if !settings.IsImageDropEnabled(ctx) {
		return transcript
	}
	return imageextract.DropImages(transcript)
}
