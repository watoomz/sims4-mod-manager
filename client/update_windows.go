//go:build windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

func applySelfUpdate() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	stage := executable + ".s4mm-update"
	pid := strconv.Itoa(os.Getpid())
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	process, err := os.StartProcess(executable, []string{executable, "--s4mm-replace", executable, stage, pid}, &os.ProcAttr{
		Files: []*os.File{devnull, devnull, devnull},
		Sys:   &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000010}, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	})
	if err != nil {
		return err
	}
	_ = process.Release()
	// The helper waits for this process to exit before touching the executable.
	os.Exit(0)
	return nil
}

func runReplacementHelper(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("invalid replacement helper arguments")
	}
	target, stage, pidText := args[0], args[1], args[2]
	pid, err := strconv.ParseUint(pidText, 10, 32)
	if err != nil {
		return err
	}
	if process, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid)); err == nil {
		_, _ = syscall.WaitForSingleObject(process, 0xffffffff)
		_ = syscall.CloseHandle(process)
	}
	backup := target + ".s4mm-rollback"
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	if err := os.Rename(stage, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	process, err := os.StartProcess(target, []string{target}, &os.ProcAttr{Files: []*os.File{nil, nil, nil}})
	if err != nil {
		_ = os.Remove(target)
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return fmt.Errorf("restart failed (%v) and rollback failed (%v)", err, rollbackErr)
		}
		return err
	}
	_ = process.Release()
	// A second helper lets the restarted program settle before deleting rollback data.
	cleanup, cleanupErr := os.StartProcess(target, []string{target, "--s4mm-replace-cleanup", backup}, &os.ProcAttr{Files: []*os.File{nil, nil, nil}})
	if cleanupErr == nil {
		_ = cleanup.Release()
	}
	return nil
}

func cleanupReplacementBackup(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("invalid cleanup helper arguments")
	}
	time.Sleep(10 * time.Second)
	return os.Remove(args[0])
}
