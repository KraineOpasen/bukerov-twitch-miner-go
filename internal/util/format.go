package util

import (
	"fmt"
	"strconv"
	"time"
)

// FormatNumber formats an integer with comma separators (e.g., 1234567 -> "1,234,567")
func FormatNumber(n int) string {
	// Format from the decimal string rather than negating n: negation would
	// overflow for math.MinInt (leaving it negative) and print garbage.
	// strconv.Itoa renders the sign and magnitude correctly for every int.
	s := strconv.Itoa(n)
	sign := ""
	if len(s) > 0 && s[0] == '-' {
		sign = "-"
		s = s[1:]
	}

	result := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result += ","
		}
		result += string(c)
	}
	return sign + result
}

// FormatDuration formats a duration into a human-readable short form (e.g., "5m", "2h", "3d")
func FormatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	if totalSeconds < 60 {
		return fmt.Sprintf("%ds", totalSeconds)
	}
	if totalSeconds < 3600 {
		return fmt.Sprintf("%dm", totalSeconds/60)
	}
	if totalSeconds < 86400 {
		return fmt.Sprintf("%dh", totalSeconds/3600)
	}
	return fmt.Sprintf("%dd", totalSeconds/86400)
}

// FormatTimeAgo formats a Unix millisecond timestamp as a relative time string
func FormatTimeAgo(timestamp int64) string {
	if timestamp == 0 {
		return "Never"
	}

	seconds := (time.Now().UnixMilli() - timestamp) / 1000
	if seconds < 60 {
		return "Just now"
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm ago", seconds/60)
	}
	if seconds < 86400 {
		return fmt.Sprintf("%dh ago", seconds/3600)
	}
	return fmt.Sprintf("%dd ago", seconds/86400)
}
