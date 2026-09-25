//go:build !linux

package supervisor

import "log/slog"

// StartReaper is a no-op outside Linux.
func StartReaper(_ *slog.Logger, _ func(pid int) bool) error {
	return nil
}
