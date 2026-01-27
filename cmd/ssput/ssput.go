package main

import (
	"fmt"
	"os"
	"time"

	"github.com/dt/gosendsafely/sendsafely"
	"github.com/dt/gosendsafely/util"
	"github.com/spf13/cobra"
)

var (
	dropzoneURL string
	dropzoneID  string
	email       string
	name        string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "ssput [flags] FILE [FILE...]",
		Short: "Upload files to a SendSafely dropzone",
		Long: `Upload files to a SendSafely dropzone.

Files are encrypted client-side and uploaded in chunks. After upload,
a secure link is returned that can be shared with recipients.

Example:
  ssput --url="https://dropzone.example.com" \
        --dropzone-id="your_dropzone_id" \
        --email="sender@example.com" \
        file1.txt file2.zip`,
		Args:         cobra.MinimumNArgs(1),
		RunE:         run,
		SilenceUsage: true,
	}

	rootCmd.Flags().StringVar(&dropzoneURL, "url", "", "Dropzone URL (required)")
	rootCmd.Flags().StringVar(&dropzoneID, "dropzone-id", "", "Dropzone ID (required)")
	rootCmd.Flags().StringVar(&email, "email", "", "Sender email (required)")
	rootCmd.Flags().StringVar(&name, "name", "", "Submitter name or ticket ID (optional, for integrations)")

	rootCmd.MarkFlagRequired("url")
	rootCmd.MarkFlagRequired("dropzone-id")
	rootCmd.MarkFlagRequired("email")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, files []string) error {
	// Validate files exist and calculate total size
	var totalSize int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("file not found: %s", f)
		}
		totalSize += info.Size()
	}

	before := time.Now()
	link, err := sendsafely.UploadToDropzone(
		dropzoneURL,
		dropzoneID,
		email,
		name,
		func(name string, size util.BytesSize, mbps util.BytesSize, frac float64) {
				fmt.Fprintf(os.Stderr, "\r%s (%s): %.1f%% (%s/s)", name, size, frac*100, mbps)
		},
		files...,
	)
	if err != nil {
		return err
	}
	dur := time.Since(before)

	fmt.Fprintf(os.Stderr, "%80s\n", ""	) // blank out last progress line.

	fmt.Fprintf(os.Stderr, "Uploaded %d files (%s)\t %s (%s/s)\n",
		len(files),util.BytesSize(totalSize), util.ConciseDuration(dur), util.BytesSize(float64(totalSize) / dur.Seconds()))

	fmt.Println(link)
	return nil
}
