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
	outDir        string
	listOnly      bool
	noKeyring     bool
	forgetKeyring bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "ssget <sendsafely-url> [file-patterns...]",
		Version: version,
		Short:   "Download files from a SendSafely package",
		Long: `Download files from a SendSafely package.

If no file patterns are specified, all files are downloaded.
Patterns use glob matching (e.g., "*.zip", "debug-*.tar.gz").`,
		Args:         cobra.MinimumNArgs(0),
		RunE:         run,
		SilenceUsage: true,
	}

	rootCmd.Flags().StringVarP(&outDir, "out", "o", ".", "Output directory")
	rootCmd.Flags().BoolVarP(&listOnly, "list", "l", false, "List files without downloading")
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

	defer util.CheckForLatestVersion("ssget", version)()

	rawURL := args[0]
	patterns := args[1:]

	credOpts := sendsafely.CredentialOptions{NoKeyring: noKeyring}
	pkg, err := sendsafely.OpenPackage(rawURL, util.Limiter(32), credOpts)
	if err != nil {
		return err
	}

	// Get list of files matching patterns
	files := pkg.Files()
	if len(patterns) > 0 {
		files = filterFiles(files, patterns)
	}

	if len(files) == 0 {
		fmt.Println("No files match the specified patterns")
		return nil
	}

	// List mode
	if listOnly {
		for _, f := range files {
			fmt.Printf("%s\t%s\t%s\n", f.Name, util.BytesSize(f.Size), f.UploadedAt)
		}
		return nil
	}

	// Download each file replicating the path structure if there are many files,
	// or just directly to the output directory if only one file.
	for _, f := range files {
		outPath := filepath.Join(outDir, f.Name)
		if len(files) == 1 {
			outPath = filepath.Join(outDir, filepath.Base(f.Name))
		}
		if s, err := os.Stat(outPath); err == nil {
			if s.Size() == int64(f.Size) {
				fmt.Printf("%s (%s) already exists\n", outPath, util.BytesSize(s.Size()))
			} else {
				fmt.Printf("%s (%s) already exists, but differs from remote size (%s) by %s\n",
					outPath, util.BytesSize(s.Size()), f.Size, f.Size-util.BytesSize(s.Size()))
			}
			continue
		}
		before := time.Now()
		fmt.Printf("%s (%s)...\n", f.Name, util.BytesSize(f.Size))
		if err := pkg.DownloadFile(f.Name, outPath, func(stage string, bps int, frac float64) {
			if frac == 0 {
				fmt.Printf("\r\t%s", stage)
			} else {
				fmt.Printf("\r\t%5s/s   %.1f%%  ", util.BytesSize(bps), frac*100)
			}
		}); err != nil {
			return fmt.Errorf("\nfailed to download %s: %w", f.Name, err)
		}
		dur := time.Since(before)
		fmt.Printf("\r%s:  %18s %5s    %5s/s%10s\n", outPath, "", util.ConciseDuration(dur), util.BytesSize(int(float64(f.Size)/dur.Seconds())), "")
	}
	return nil
}

func filterFiles(files []sendsafely.FileInfo, patterns []string) []sendsafely.FileInfo {
	var result []sendsafely.FileInfo
	for _, f := range files {
		for _, p := range patterns {
			if matched, _ := filepath.Match(p, f.Name); matched {
				result = append(result, f)
				break
			}
		}
	}
	return result
}
