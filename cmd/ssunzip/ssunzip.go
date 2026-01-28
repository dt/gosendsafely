package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dt/gosendsafely/sendsafely"
	"github.com/dt/gosendsafely/util"
	"github.com/dt/gosendsafely/ziputil"
	"github.com/spf13/cobra"
)

var version = "dev"

var (
	outDir        string
	zipFile       string
	listOnly      bool
	excludes      []string
	noKeyring     bool
	forgetKeyring bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "ssunzip <sendsafely-url> [file-patterns...]",
		Version: version,
		Long: `Extract files from a ZIP stored in a SendSafely package.

If no file patterns are specified, all files are extracted.
Patterns use glob matching (e.g., "*.json", "debug/nodes/*/logs/*").`,
		Args:         cobra.MinimumNArgs(0),
		RunE:         run,
		SilenceUsage: true,
	}

	rootCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: ZIP filename without .zip)")
	rootCmd.Flags().StringVarP(&zipFile, "zip-file", "z", "", "Name of ZIP file in package (auto-detected if only one)")
	rootCmd.Flags().BoolVarP(&listOnly, "list", "l", false, "List files without extracting")
	rootCmd.Flags().StringArrayVarP(&excludes, "exclude", "x", nil, "Exclude files matching pattern")
	rootCmd.Flags().BoolVar(&noKeyring, "no-keyring", false, "Don't use system keychain for credentials")
	rootCmd.Flags().BoolVar(&forgetKeyring, "forget-keyring", false, "Remove saved credentials from system keychain")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// Handle --forget-keyring
	if forgetKeyring {
		if err := sendsafely.ForgetCredentials(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Credentials removed from system keychain.")
		if len(args) == 0 {
			return nil
		}
	}

	if len(args) == 0 {
		return fmt.Errorf("sendsafely-url is required")
	}

	defer util.CheckForLatestVersion("ssunzip", version)()

	rawURL := args[0]
	patterns := args[1:]
	lim := util.Limiter(32)

	credOpts := sendsafely.CredentialOptions{NoKeyring: noKeyring}
	pkg, err := sendsafely.OpenPackage(rawURL, lim, credOpts)
	if err != nil {
		return err
	}

	// Find the ZIP file
	zipFileName := zipFile
	if zipFileName == "" {
		zipFileName, err = findZipFile(pkg)
		if err != nil {
			return err
		}
	}

	// Set default output directory from ZIP filename
	if outDir == "" {
		outDir = strings.TrimSuffix(zipFileName, ".zip")
		outDir = strings.TrimSuffix(outDir, ".ZIP")
	}

	fmt.Printf("Opening %s...\n", zipFileName)

	// Open the ZIP file
	zip, err := pkg.Open(zipFileName)
	if err != nil {
		return fmt.Errorf("failed to open ZIP: %w", err)
	}
	fmt.Printf("ZIP file size: %s\n", util.BytesSize(zip.Size()))

	// Get index of files in ZIP
	index, err := ziputil.DecodeIndex(zip)
	if err != nil {
		return fmt.Errorf("failed to parse ZIP index: %w", err)
	}

	// Strip common folder prefix if all entries share one
	if prefix := index.StripCommonPrefix(); prefix != "" {
		fmt.Printf("Stripping common prefix: %s\n", prefix)
	}

	totalSize, totalCount := 0, 0
	for _, e := range index {
		totalSize += int(e.Size)
		totalCount++
	}

	if len(patterns) > 0 || len(excludes) > 0 {
		index = index.Filtered(patterns, excludes)
	}

	filteredSize, filteredCount := 0, 0
	var filteredCompressedSize util.BytesSize
	for _, e := range index {
		filteredSize += int(e.Size)
		filteredCompressedSize += e.CompressedSize()
		filteredCount++
	}

	if len(index) == 0 {
		fmt.Println("No files match the specified patterns")
		return nil
	}

	// List mode
	if listOnly {
		if len(patterns) > 0 || len(excludes) > 0 {
			fmt.Printf("%d of %d files match:\n", filteredCount, totalCount)
		} else {
			fmt.Printf("Content:\n")
		}
		for _, e := range index {
			fmt.Printf("%s\t%s\t(%s compressed)\n", e.Name, util.BytesSize(e.Size), util.BytesSize(int64(e.CompressedSize())))
		}
		fmt.Printf("Total: %d files\t%s\t(%s compressed)\n", filteredCount, util.BytesSize(int64(filteredSize)), util.BytesSize(int64(filteredCompressedSize)))
		return nil
	}

	// Create output directory
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	fmt.Printf("Fetching and extracting %d files to %s...\n", filteredCount, outDir)
	before := time.Now()
	skipped, skippedBytes, err := ziputil.Extract(zip, index, outDir, lim, func(frac float64, rate util.BytesSize) {
		fmt.Printf("\r%0.1f%%     (%s/s)   ", frac*100, rate)
	})
	if err != nil {
		return fmt.Errorf("\nextraction failed: %w", err)
	}
	dur := time.Since(before)
	rate := util.BytesSize(float64(filteredCompressedSize-skippedBytes) / dur.Seconds())

	if skipped > 0 {
		fmt.Printf("\rFetched %d files / %s  to %s (%d  / %s already existed) in %s (%s/s) %10s\n",
			filteredCount-skipped, util.BytesSize(filteredSize-int(skippedBytes)), outDir, skipped, util.BytesSize(int64(skippedBytes)), util.ConciseDuration(dur), rate, "")
	} else {
		fmt.Printf("\rFetched %d files / %s  to %s in %s (%s/s)%30s\n",
			filteredCount, util.BytesSize(filteredSize), outDir, util.ConciseDuration(dur), rate, "")
	}
	return nil

}

func findZipFile(pkg *sendsafely.Package) (string, error) {
	files := pkg.Files()
	var zips []string
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
			zips = append(zips, f.Name)
		}
	}

	if len(zips) == 0 {
		return "", fmt.Errorf("no ZIP files found in package")
	}
	if len(zips) > 1 {
		return "", fmt.Errorf("multiple ZIP files in package, use --zip-file to specify: %v", zips)
	}
	return zips[0], nil
}
