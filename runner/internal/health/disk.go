package health

import (
	"errors"
	"fmt"
	"syscall"
)

func AvailableBytes(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return stats.Bavail * uint64(stats.Bsize), nil
}

func RequireFreeBytes(path string, minimum uint64, available func(string) (uint64, error)) error {
	if minimum == 0 || available == nil {
		return errors.New("disk health requirement is invalid")
	}
	free, err := available(path)
	if err != nil {
		return fmt.Errorf("inspect runner disk space: %w", err)
	}
	if free < minimum {
		return fmt.Errorf("runner free disk space %d is below required %d", free, minimum)
	}
	return nil
}
