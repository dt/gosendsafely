package main

import (
	"fmt"
	"os"

	"github.com/dt/ssdownload/ss"
	"github.com/spf13/cobra"
)

var (
	dropzoneURL string
	dropzoneID  string
	email       string
	label       string
	verbose     bool
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
	rootCmd.Flags().StringVar(&label, "label", "", "Package label (optional, for ticketing integration)")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")

	rootCmd.MarkFlagRequired("url")
	rootCmd.MarkFlagRequired("dropzone-id")
	rootCmd.MarkFlagRequired("email")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, files []string) error {
	// Validate files exist before starting
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("file not found: %s", f)
		}
	}

	client := ss.NewDropzoneClient(dropzoneURL, dropzoneID)

	if verbose {
		fmt.Fprintf(os.Stderr, "Creating package...\n")
	}

	pkg, err := client.CreatePackage()
	if err != nil {
		return fmt.Errorf("failed to create package: %w", err)
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "Package created: %s\n", pkg.PackageCode)
	}

	for _, f := range files {
		info, _ := os.Stat(f)
		if verbose {
			fmt.Fprintf(os.Stderr, "Uploading %s (%s)...\n", f, ss.BytesSize(info.Size()))
		}

		_, err := pkg.AddFilePath(f, func(p ss.UploadProgress) {
			pct := float64(p.Bytes) / float64(p.Total) * 100
			fmt.Fprintf(os.Stderr, "\r%s: %d/%d parts (%.1f%%)", p.FileName, p.Part, p.Parts, pct)
		})
		if err != nil {
			return fmt.Errorf("failed to upload %s: %w", f, err)
		}
		fmt.Fprintln(os.Stderr, " done")
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "Finalizing package...\n")
	}

	link, err := pkg.Finalize(email)
	if err != nil {
		return fmt.Errorf("failed to finalize package: %w", err)
	}

	if label != "" {
		if verbose {
			fmt.Fprintf(os.Stderr, "Submitting to dropzone with label %q...\n", label)
		}
		if err := pkg.SubmitHostedDropzone(email, label); err != nil {
			fmt.Fprintf(os.Stderr, "warning: dropzone submission failed: %v\n", err)
		}
	}

	fmt.Println(link)
	return nil
}
