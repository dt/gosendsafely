package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dt/ssdownload/ss"
	"github.com/spf13/cobra"
)

var (
	outDir        string
	listOnly      bool
	noKeyring     bool
	forgetKeyring bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "ssget <sendsafely-url> [file-patterns...]",
		Short: "Download files from a SendSafely package",
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
		if err := ss.ForgetCredentials(); err != nil {
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

	rawURL := args[0]
	patterns := args[1:]

	credOpts := ss.CredentialOptions{NoKeyring: noKeyring}
	pkg, err := ss.OpenPackage(rawURL, ss.Limiter(16), credOpts)
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
			fmt.Printf("%s\t%s\n", f.Name, ss.BytesSize(f.Size))
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
				fmt.Printf("%s (%s) already exists\n", outPath, ss.BytesSize(s.Size()))
			} else {
				fmt.Printf("%s (%s) already exists, but differs from remote size (%s) by %s\n",
					outPath, ss.BytesSize(s.Size()), f.Size, f.Size-ss.BytesSize(s.Size()))
			}
			continue
		}
		before := time.Now()
		if err := pkg.DownloadFile(f.Name, outPath, func(stage string, bps int, frac float64) {
			if frac == 0 {
				fmt.Printf("\r%s (%s):  %18s", f.Name, ss.BytesSize(f.Size), stage)
			} else {
				fmt.Printf("\r%s (%s):  %18s\t%5s/s   %.1f%%    ", f.Name, ss.BytesSize(f.Size), stage, ss.BytesSize(bps), frac*100)
			}
		}); err != nil {
			return fmt.Errorf("failed to download %s: %w", f.Name, err)
		}
		dur := time.Since(before) 
		fmt.Printf("\r%s:  %18s %5s    %5s/s%10s\n", outPath, "", ss.ConciseDuration(dur), ss.BytesSize(int(float64(f.Size)/dur.Seconds())), "")
	}
	return nil
}

func filterFiles(files []ss.FileInfo, patterns []string) []ss.FileInfo {
	var result []ss.FileInfo
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
