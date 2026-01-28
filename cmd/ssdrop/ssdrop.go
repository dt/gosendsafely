package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dt/gosendsafely/sendsafely"
	"github.com/dt/gosendsafely/util"
	"github.com/spf13/cobra"
)

var version = "dev"

var (
	dropzoneURL string
	dropzoneID  string
	email       string
	name        string
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "ssdrop [flags] FILE [FILE...]",
		Version: version,
		Short:   "Upload files to a SendSafely dropzone",
		Long: `Upload files to a SendSafely dropzone.

This tool uploads files to SendSafely dropzones (anonymous upload portals).
It does not require API credentials - only the dropzone URL and ID.

Files are encrypted client-side and uploaded in chunks. After upload,
a secure link is returned that can be shared with recipients.

Example:
  ssdrop --url="https://dropzone.example.com" \
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
	defer util.CheckForLatestVersion("ssdrop", version)()

	// Validate files exist and calculate total size
	var totalSize int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("file not found: %s", f)
		}
		totalSize += info.Size()
	}

	// Print what we're uploading
	if len(files) == 1 {
		fmt.Fprintf(os.Stderr, "Uploading %s (%s) to %s...\n",
			filepath.Base(files[0]), util.BytesSize(totalSize), dropzoneURL)
	} else {
		fmt.Fprintf(os.Stderr, "Uploading %d files (%s) to %s...\n",
			len(files), util.BytesSize(totalSize), dropzoneURL)
	}

	before := time.Now()
	link, err := sendsafely.UploadToDropzone(
		dropzoneURL,
		dropzoneID,
		email,
		name,
		func(name string, size util.BytesSize, mbps util.BytesSize, frac float64) {
			fmt.Fprintf(os.Stderr, "\r%-60s", fmt.Sprintf("%s (%s): %.1f%% (%s/s)", name, size, frac*100, mbps))
		},
		files...,
	)
	if err != nil {
		return err
	}
	dur := time.Since(before)

	fmt.Fprintf(os.Stderr, "\r%80s\r", "") // blank out last progress line

	fmt.Fprintf(os.Stderr, "Uploaded %d files (%s)\t %s (%s/s)\n",
		len(files), util.BytesSize(totalSize), util.ConciseDuration(dur), util.BytesSize(float64(totalSize)/dur.Seconds()))

	fmt.Println(link)
	return nil
}
