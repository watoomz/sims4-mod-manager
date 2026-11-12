//go:build !windows

package main

import "errors"

func applySelfUpdate() error {
	return errors.New("automatic executable replacement is supported on Windows clients only")
}

func runReplacementHelper([]string) error {
	return errors.New("Windows update helper is unavailable on this platform")
}
func cleanupReplacementBackup([]string) error { return nil }
