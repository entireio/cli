package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	_ "image/png"  // register the PNG decoder for image.DecodeConfig
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// trailAttachmentMaxBytes is the server's attachment cap (10 MiB); a larger
// file is refused before anything is registered.
const trailAttachmentMaxBytes = 10 << 20

// trailAttachmentInlineTypes are the content types a trail description
// renders inline, the only ones an `attachment:` reference accepts. The type
// is detected from the file's bytes: the server trusts the declared type.
var trailAttachmentInlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// trailAttachmentUploadClient PUTs the bytes to the presigned URL. It is not
// the API client: the URL is pre-authorized and must not receive the
// caller's token. A test replaces it.
var trailAttachmentUploadClient = httpclient.New(httpclient.Options{Timeout: 5 * time.Minute})

// trailAttachmentMarkdown is the description reference for an attachment, in
// the form the server renders back on read, so a body round-trips unchanged.
func trailAttachmentMarkdown(id string) string {
	return "![image](attachment:" + id + ")"
}

func newProjectTrailAttachmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachment",
		Short: "Upload, list, and remove images for a trail's description",
		Long: `Upload, list, and remove the images a trail's description shows.

An uploaded image is referenced from the description by a line of its own:

  ![image](attachment:<id>)

'entire trail attachment add' prints that line; put it in the description with
'entire trail update --body'. PNG, JPEG, GIF, and WebP images up to 10 MiB are
accepted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.PersistentFlags().String("trail", "", "Project trail number, ID, or <repo>/<number> (defaults to the current branch's parent)")
	cmd.PersistentFlags().String("branch", "", "Discover the trail through this repository branch")
	cmd.AddCommand(newProjectTrailAttachmentAddCmd(), newProjectTrailAttachmentListCmd(), newProjectTrailAttachmentRemoveCmd())
	return cmd
}

func newProjectTrailAttachmentAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Upload an image and print its description reference",
		Long: `Upload a PNG, JPEG, GIF, or WebP image (up to 10 MiB) to the trail and print the
line that shows it in the description. The type is read from the file's
contents, not its name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			file, err := readTrailAttachmentFile(args[0])
			if err != nil {
				return err
			}
			target, err := resolveProjectTrail(cmd, trailSubcommandSelector(cmd))
			if err != nil {
				return err
			}
			attachment, err := uploadTrailAttachment(cmd.Context(), target, file)
			if err != nil {
				return err
			}
			ref := trailAttachmentMarkdown(attachment.ID)
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), struct {
					Attachment api.ProjectTrailAttachment `json:"attachment"`
					Markdown   string                     `json:"markdown"`
				}{attachment, ref})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Uploaded %s to %s as %s\n", tuiutil.SanitizeTerminalLabel(file.name), target.label(), tuiutil.SanitizeTerminalLabel(attachment.ID))
			fmt.Fprintln(w, "Show it in the description with this line on its own:")
			fmt.Fprintf(w, "  %s\n", tuiutil.SanitizeTerminalLabel(ref))
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// trailAttachmentFile is a local image checked for upload.
type trailAttachmentFile struct {
	name          string
	contentType   string
	data          []byte
	width, height int
}

// readTrailAttachmentFile reads an explicit user-named file and accepts it only
// when its bytes are an inline image type within the size cap.
func readTrailAttachmentFile(path string) (trailAttachmentFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return trailAttachmentFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return trailAttachmentFile{}, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > trailAttachmentMaxBytes {
		return trailAttachmentFile{}, fmt.Errorf("%s is %d bytes; attachments are limited to %d bytes (10 MiB)", path, info.Size(), trailAttachmentMaxBytes)
	}
	data, err := os.ReadFile(path) //nolint:gosec // an explicit path the user named
	if err != nil {
		return trailAttachmentFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return trailAttachmentFile{}, fmt.Errorf("%s is empty", path)
	}
	if len(data) > trailAttachmentMaxBytes {
		return trailAttachmentFile{}, fmt.Errorf("%s is larger than the 10 MiB attachment limit", path)
	}
	contentType := http.DetectContentType(data)
	if !trailAttachmentInlineTypes[contentType] {
		return trailAttachmentFile{}, fmt.Errorf("%s is %s; only PNG, JPEG, GIF, and WebP images can be shown in a trail description", path, contentType)
	}
	file := trailAttachmentFile{name: filepath.Base(path), contentType: contentType, data: data}
	// Dimensions are optional hints; WebP has no decoder in the standard
	// library, and a file that does not decode is still the server's to keep.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
		file.width, file.height = cfg.Width, cfg.Height
	}
	return file, nil
}

// uploadTrailAttachment registers the upload, PUTs the bytes to the presigned
// URL, and completes it.
func uploadTrailAttachment(ctx context.Context, target *projectTrailTarget, file trailAttachmentFile) (api.ProjectTrailAttachment, error) {
	collection := target.path() + "/attachments"
	var intent api.ProjectTrailAttachmentUpload
	headers := http.Header{"Idempotency-Key": []string{uuid.NewString()}}
	body := api.ProjectTrailAttachmentIntent{Filename: file.name, ContentType: file.contentType, Width: file.width, Height: file.height}
	if _, err := target.Client.ProjectTrailRequest(ctx, http.MethodPost, collection, body, headers, &intent); err != nil {
		return api.ProjectTrailAttachment{}, fmt.Errorf("register attachment: %w", err)
	}
	if !looksLikeULID(intent.ID) {
		return api.ProjectTrailAttachment{}, errors.New("register attachment: the server returned no attachment ID")
	}
	// From here a failure would leave a registered attachment that never
	// completes, so delete it before reporting the failure.
	fail := func(err error) (api.ProjectTrailAttachment, error) {
		if _, delErr := target.Client.ProjectTrailRequest(context.WithoutCancel(ctx), http.MethodDelete, collection+"/"+url.PathEscape(intent.ID), nil, nil, nil); delErr != nil {
			return api.ProjectTrailAttachment{}, fmt.Errorf("%w (the unfinished attachment %s could not be removed: %w)", err, intent.ID, delErr)
		}
		return api.ProjectTrailAttachment{}, err
	}
	if intent.Upload.MaxBytes > 0 && int64(len(file.data)) > intent.Upload.MaxBytes {
		return fail(fmt.Errorf("%s is larger than the server's %d-byte limit", file.name, intent.Upload.MaxBytes))
	}
	if !intent.Upload.ExpiresAt.IsZero() && !time.Now().Before(intent.Upload.ExpiresAt) {
		return fail(errors.New("upload attachment: the upload URL had already expired"))
	}
	if err := putTrailAttachmentBytes(ctx, intent.Upload.URL, file); err != nil {
		return fail(err)
	}
	var done api.ProjectTrailAttachmentComplete
	if _, err := target.Client.ProjectTrailRequest(ctx, http.MethodPost, collection+"/"+url.PathEscape(intent.ID)+"/complete", nil, nil, &done); err != nil {
		return fail(fmt.Errorf("complete attachment %s: %w", intent.ID, err))
	}
	if done.Attachment.ID == "" {
		done.Attachment.ID = intent.ID
	}
	return done.Attachment, nil
}

// putTrailAttachmentBytes uploads to the presigned URL with the declared
// Content-Type. The URL must be HTTPS, except on loopback for tests.
func putTrailAttachmentBytes(ctx context.Context, rawURL string, file trailAttachmentFile) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return errors.New("upload attachment: the server returned no usable upload URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("upload attachment: refusing a non-HTTPS upload URL (%s)", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(file.data))
	if err != nil {
		return fmt.Errorf("upload attachment: %w", err)
	}
	req.Header.Set("Content-Type", file.contentType)
	req.ContentLength = int64(len(file.data))
	resp, err := trailAttachmentUploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload attachment: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck // drain for reuse; the status decides
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("upload attachment: storage answered %s", resp.Status)
	}
	return nil
}

func newProjectTrailAttachmentListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdList,
		Short: "List a trail's attachments",
		Long:  "List a trail's attachments, including uploads that never completed, with each one's description reference.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			target, err := resolveProjectTrail(cmd, trailSubcommandSelector(cmd))
			if err != nil {
				return err
			}
			var out api.ProjectTrailAttachmentList
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodGet, target.path()+"/attachments", nil, nil, &out); err != nil {
				return fmt.Errorf("list attachments: %w", err)
			}
			if out.Attachments == nil {
				out.Attachments = []api.ProjectTrailAttachment{}
			}
			if jsonRequested(cmd) {
				return printJSON(cmd.OutOrStdout(), out)
			}
			if len(out.Attachments) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No attachments on %s\n", target.label())
				return nil
			}
			return printTable(cmd.OutOrStdout(), []string{"ID", "FILENAME", "TYPE", "SIZE", "READY", "REFERENCE"}, out.Attachments, func(a api.ProjectTrailAttachment) []string {
				ref := ""
				if a.IsReady && trailAttachmentInlineTypes[a.ContentType] {
					ref = trailAttachmentMarkdown(a.ID)
				}
				return []string{tuiutil.SanitizeTerminalLabel(a.ID), tuiutil.SanitizeTerminalLabel(a.Filename), tuiutil.SanitizeTerminalLabel(a.ContentType),
					strconv.FormatInt(a.SizeBytes, 10), strconv.FormatBool(a.IsReady), tuiutil.SanitizeTerminalLabel(ref)}
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newProjectTrailAttachmentRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <attachment-id>",
		Short: "Delete an attachment",
		Long: `Delete an attachment from the trail. A description line that still references it
stops showing the image. Deleting cannot be undone, so it requires --force.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			id := strings.TrimSpace(args[0])
			if !looksLikeULID(id) {
				return fmt.Errorf("expected an attachment ID (see 'entire trail attachment list'), got %q", tuiutil.SanitizeTerminalLabel(id))
			}
			if !force {
				return errors.New("deleting an attachment requires --force")
			}
			target, err := resolveProjectTrail(cmd, trailSubcommandSelector(cmd))
			if err != nil {
				return err
			}
			if _, err := target.Client.ProjectTrailRequest(cmd.Context(), http.MethodDelete, target.path()+"/attachments/"+url.PathEscape(id), nil, nil, nil); err != nil {
				return fmt.Errorf("delete attachment %s: %w", id, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted attachment %s from %s\n", id, target.label())
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Confirm the deletion")
	return cmd
}
