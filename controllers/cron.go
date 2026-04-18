package controllers

import (
	"errors"
	"fmt"
	"time"

	internalprobes "github.com/loks0n/synthetics-operator/internal/probes"
)

// validSchedule returns the cron expression and true, or ("", false) for invalid intervals.
func validSchedule(namespace, name string, interval time.Duration) (string, bool) {
	s, err := intervalToCron(namespace, name, interval)
	return s, err == nil
}

// intervalToCron converts a duration to a cron schedule expression, applying
// a per-probe offset derived from the probe's namespace and name to prevent
// multiple tests with the same interval from firing simultaneously.
//
// Valid intervals: 1m–30m (must evenly divide 60), 1h–12h (must evenly divide 24), 24h.
func intervalToCron(namespace, name string, interval time.Duration) (string, error) {
	totalMinutes := int(interval.Minutes())
	if totalMinutes < 1 {
		return "", errors.New("interval must be at least 1m (cron resolution)")
	}

	offsetMinutes := int(internalprobes.ProbeOffset(namespace, name, interval).Minutes())

	switch {
	case totalMinutes == 1:
		return "* * * * *", nil
	case totalMinutes < 60:
		if 60%totalMinutes != 0 {
			return "", errors.New("sub-hour intervals must evenly divide 60 (valid: 2m, 3m, 4m, 5m, 6m, 10m, 12m, 15m, 20m, 30m)")
		}
		return fmt.Sprintf("%d/%d * * * *", offsetMinutes%totalMinutes, totalMinutes), nil
	case totalMinutes%60 != 0:
		return "", errors.New("intervals >= 1h must be whole hours")
	default:
		hours := totalMinutes / 60
		if hours > 24 || 24%hours != 0 {
			return "", errors.New("hour intervals must evenly divide 24 (valid: 1h, 2h, 3h, 4h, 6h, 8h, 12h, 24h)")
		}
		if hours == 24 {
			return fmt.Sprintf("%d %d * * *", offsetMinutes%60, (offsetMinutes/60)%24), nil
		}
		return fmt.Sprintf("%d */%d * * *", offsetMinutes%60, hours), nil
	}
}
