//go:build linux || darwin

package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func Acquire() (*Lock, error) {
	runtimeDir, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(runtimeDir, "swarmfolio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open run lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("another applied Swarmfolio run is already in progress")
		}
		return nil, fmt.Errorf("acquire run lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func (lock *Lock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
