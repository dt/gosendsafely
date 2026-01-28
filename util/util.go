package util

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Sem is a counting semaphore for limiting concurrent operations.
type Sem chan struct{}

// Limiter creates a semaphore that limits concurrency to n operations.
func Limiter(n int) Sem {
	return make(Sem, n)
}

// Acquire blocks until a slot is available or the context is cancelled.
// Callers should check ctx.Err() if they need to distinguish between the two.
func (l Sem) Acquire(ctx context.Context) {
	if l != nil {
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
		}
	}
}

func (l Sem) Release() {
	if l != nil {
		select {
		case <-l:
		default:
		}
	}
}

// BytesSize is a byte count that formats with human-readable units (KB, MB, etc).
type BytesSize int

// Format implements fmt.Formatter to display byte sizes with appropriate units.
func (n BytesSize) Format(f fmt.State, c rune) {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(n)
	unit := 0
	for math.Abs(size) >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	fmt.Fprintf(f, "%.2f %s", size, units[unit])
}

// ConciseDuration is a duration that formats concisely (e.g., "1m30s").
type ConciseDuration time.Duration

// Format implements fmt.Formatter to display durations in a compact format.
func (d ConciseDuration) Format(f fmt.State, c rune) {
	s := time.Duration(d).Seconds()
	if s < 60 {
		fmt.Fprintf(f, "%0.1fs", s)
		return
	}
	m := int(s / 60)
	if m < 60 {
		fmt.Fprintf(f, "%dm%ds", m, int(s)%60)
		return
	}
	fmt.Fprintf(f, "%dh%dm", m/60, m%60)
}

const releaseURL = "https://api.github.com/repos/dt/gosendsafely/releases/latest"

// CheckForLatestVersion starts a background check for newer versions.
// Returns a function that should be deferred/run when main would exit that
// cancels the check if still running or prints a notice if a newer version was
// found.
func CheckForLatestVersion(tool, currentVersion string) func() {
	// Skip check for dev builds
	if currentVersion == "dev" || currentVersion == "" {
		return func() {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	result := make(chan string, 1)

	go func() {
		defer close(result)

		req, err := http.NewRequestWithContext(ctx, "GET", releaseURL, nil)
		if err != nil {
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			return
		}
		defer resp.Body.Close()

		var release struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			return
		}

		latest := strings.TrimPrefix(release.TagName, "v")
		current := strings.TrimPrefix(currentVersion, "v")
		if latest != current {
			result <- fmt.Sprintf("\n%s: version %s is available (current: %s)", tool, release.TagName, currentVersion)
		}
	}()

	return func() {
		cancel()
		select {
		case msg := <-result:
			if msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
		default:
		}
	}
}
