package ss

import (
	"fmt"
	"testing"
	"time"
)

// TestBytesSize_Format tests BytesSize formatting.
func TestBytesSize_Format(t *testing.T) {
	testCases := []struct {
		size     BytesSize
		expected string
	}{
		{0, "0.00 B"},
		{1, "1.00 B"},
		{100, "100.00 B"},
		{1023, "1023.00 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TB"},
		{1024 * 1024 * 1024 * 1024 * 2, "2.00 TB"},
		{-1024, "-1.00 KB"},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%d", tc.size), func(t *testing.T) {
			result := fmt.Sprintf("%s", tc.size)
			if result != tc.expected {
				t.Errorf("BytesSize(%d) = %q, want %q", tc.size, result, tc.expected)
			}
		})
	}
}

// TestBytesSize_LargeValues tests BytesSize with very large values.
func TestBytesSize_LargeValues(t *testing.T) {
	// 5 TB
	size := BytesSize(5 * 1024 * 1024 * 1024 * 1024)
	result := fmt.Sprintf("%s", size)
	if result != "5.00 TB" {
		t.Errorf("Expected '5.00 TB', got %q", result)
	}
}

// TestConciseDuration_Seconds tests ConciseDuration for sub-minute durations.
func TestConciseDuration_Seconds(t *testing.T) {
	testCases := []struct {
		dur      time.Duration
		expected string
	}{
		{0, "0.0s"},
		{500 * time.Millisecond, "0.5s"},
		{1 * time.Second, "1.0s"},
		{30 * time.Second, "30.0s"},
		{59 * time.Second, "59.0s"},
		{59*time.Second + 500*time.Millisecond, "59.5s"},
	}

	for _, tc := range testCases {
		t.Run(tc.dur.String(), func(t *testing.T) {
			result := fmt.Sprintf("%s", ConciseDuration(tc.dur))
			if result != tc.expected {
				t.Errorf("ConciseDuration(%s) = %q, want %q", tc.dur, result, tc.expected)
			}
		})
	}
}

// TestConciseDuration_Minutes tests ConciseDuration for durations >= 1 minute but < 1 hour.
func TestConciseDuration_Minutes(t *testing.T) {
	testCases := []struct {
		dur      time.Duration
		expected string
	}{
		{1 * time.Minute, "1m0s"},
		{1*time.Minute + 30*time.Second, "1m30s"},
		{5 * time.Minute, "5m0s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
	}

	for _, tc := range testCases {
		t.Run(tc.dur.String(), func(t *testing.T) {
			result := fmt.Sprintf("%s", ConciseDuration(tc.dur))
			if result != tc.expected {
				t.Errorf("ConciseDuration(%s) = %q, want %q", tc.dur, result, tc.expected)
			}
		})
	}
}

// TestConciseDuration_Hours tests ConciseDuration for durations >= 1 hour.
func TestConciseDuration_Hours(t *testing.T) {
	testCases := []struct {
		dur      time.Duration
		expected string
	}{
		{1 * time.Hour, "1h0m"},
		{1*time.Hour + 30*time.Minute, "1h30m"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{24 * time.Hour, "24h0m"},
	}

	for _, tc := range testCases {
		t.Run(tc.dur.String(), func(t *testing.T) {
			result := fmt.Sprintf("%s", ConciseDuration(tc.dur))
			if result != tc.expected {
				t.Errorf("ConciseDuration(%s) = %q, want %q", tc.dur, result, tc.expected)
			}
		})
	}
}

// TestBytesSize_ZeroAndNegative tests edge cases.
func TestBytesSize_ZeroAndNegative(t *testing.T) {
	zero := BytesSize(0)
	if fmt.Sprintf("%s", zero) != "0.00 B" {
		t.Errorf("Expected '0.00 B' for zero, got %q", fmt.Sprintf("%s", zero))
	}

	// Negative values stay in bytes (since Abs is only used for unit comparison)
	negative := BytesSize(-512)
	result := fmt.Sprintf("%s", negative)
	if result != "-512.00 B" {
		t.Errorf("Expected '-512.00 B' for -512, got %q", result)
	}
}

// TestConciseDuration_Zero tests zero duration.
func TestConciseDuration_Zero(t *testing.T) {
	d := ConciseDuration(0)
	result := fmt.Sprintf("%s", d)
	if result != "0.0s" {
		t.Errorf("Expected '0.0s' for zero duration, got %q", result)
	}
}

// TestBytesSize_BoundaryValues tests values at unit boundaries.
func TestBytesSize_BoundaryValues(t *testing.T) {
	testCases := []struct {
		name     string
		size     BytesSize
		expected string
	}{
		{"1 byte below KB", 1023, "1023.00 B"},
		{"exactly 1 KB", 1024, "1.00 KB"},
		{"1 byte above KB", 1025, "1.00 KB"},
		{"1 byte below MB", 1024*1024 - 1, "1024.00 KB"},
		{"exactly 1 MB", 1024 * 1024, "1.00 MB"},
		{"1 byte above MB", 1024*1024 + 1, "1.00 MB"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := fmt.Sprintf("%s", tc.size)
			if result != tc.expected {
				t.Errorf("BytesSize(%d) = %q, want %q", tc.size, result, tc.expected)
			}
		})
	}
}

// TestConciseDuration_BoundaryValues tests values at unit boundaries.
func TestConciseDuration_BoundaryValues(t *testing.T) {
	testCases := []struct {
		name     string
		dur      time.Duration
		expected string
	}{
		{"just under 1 minute", 59*time.Second + 999*time.Millisecond, "60.0s"},
		{"exactly 1 minute", 60 * time.Second, "1m0s"},
		{"just over 1 minute", 60*time.Second + 1*time.Millisecond, "1m0s"},
		{"just under 1 hour", 59*time.Minute + 59*time.Second, "59m59s"},
		{"exactly 1 hour", 60 * time.Minute, "1h0m"},
		{"just over 1 hour", 60*time.Minute + 1*time.Second, "1h0m"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := fmt.Sprintf("%s", ConciseDuration(tc.dur))
			if result != tc.expected {
				t.Errorf("ConciseDuration(%s) = %q, want %q", tc.dur, result, tc.expected)
			}
		})
	}
}
