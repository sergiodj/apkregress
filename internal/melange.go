// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright 2025 Chainguard, Inc.

package internal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type MelangeClient struct {
	repoPath    string
	verbose     bool
	logDir      string
	hangTimeout time.Duration
}

// ErrPackageYAMLNotFound indicates that the package YAML file doesn't exist
var ErrPackageYAMLNotFound = errors.New("package YAML file not found")

// ErrTestHung indicates that a test exceeded the timeout and was killed
var ErrTestHung = errors.New("test hung and was killed after timeout")

func NewMelangeClient(repoPath string, verbose bool, logDir string, hangTimeout time.Duration) *MelangeClient {
	return &MelangeClient{
		repoPath:    repoPath,
		verbose:     verbose,
		logDir:      logDir,
		hangTimeout: hangTimeout,
	}
}

// runMakeTarget runs `make <makeTarget>` in repoPath for packageName.
// logTag is inserted between package name and with/without_repo in log filenames;
// pass an empty string to keep backward-compatible naming (e.g. "foo_with_repo.log").
func (m *MelangeClient) runMakeTarget(packageName, makeTarget, logTag string, withRepo bool, apkRepo string) error {
	// Check if the package YAML file exists
	yamlFilePath := filepath.Join(m.repoPath, fmt.Sprintf("%s.yaml", packageName))
	if _, err := os.Stat(yamlFilePath); os.IsNotExist(err) {
		if m.verbose {
			fmt.Printf("Skipping %s: YAML file not found at %s\n", packageName, yamlFilePath)
		}
		return ErrPackageYAMLNotFound
	}

	// Create temporary directory for build
	tempDir, err := os.MkdirTemp("/tmp", fmt.Sprintf("melange-build-%s-", packageName))
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	var cmd *exec.Cmd

	// Create log file name
	repoLabel := map[bool]string{true: "with_repo", false: "without_repo"}[withRepo]
	var logFileName string
	if logTag == "" {
		logFileName = fmt.Sprintf("%s_%s.log", packageName, repoLabel)
	} else {
		logFileName = fmt.Sprintf("%s_%s_%s.log", packageName, logTag, repoLabel)
	}
	logFilePath := filepath.Join(m.logDir, logFileName)

	// Create and open log file
	logFile, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to create log file %s: %w", logFilePath, err)
	}
	defer logFile.Close()

	if withRepo {
		if m.verbose {
			fmt.Printf("Running make %s for %s with APK repository: %s (temp: %s, log: %s)\n",
				makeTarget, packageName, apkRepo, tempDir, logFilePath)
		}
		cmd = exec.Command("make", makeTarget)
		extraOpts := fmt.Sprintf("--repository-append %s", apkRepo)
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("MELANGE_EXTRA_OPTS=%s", extraOpts),
			fmt.Sprintf("TMPDIR=%s", tempDir))
	} else {
		if m.verbose {
			fmt.Printf("Running make %s for %s without APK repository (temp: %s, log: %s)\n",
				makeTarget, packageName, tempDir, logFilePath)
		}
		cmd = exec.Command("make", makeTarget)
		cmd.Env = append(os.Environ(), fmt.Sprintf("TMPDIR=%s", tempDir))
	}

	cmd.Dir = m.repoPath
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Set up process group so we can kill all child processes on timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Create context with configurable timeout
	ctx, cancel := context.WithTimeout(context.Background(), m.hangTimeout)
	defer cancel()

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start make %s: %w", makeTarget, err)
	}

	// Channel to capture the result of cmd.Wait()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for either completion or timeout
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("make %s failed: %w", makeTarget, err)
		}
		return nil
	case <-ctx.Done():
		// Timeout occurred, kill the entire process group
		if cmd.Process != nil {
			// Kill the entire process group to ensure all child processes are terminated
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err == nil {
				// Kill the process group (negative PID kills the group)
				syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				// Fallback to killing just the main process
				cmd.Process.Kill()
			}
		}
		// Wait for the process to actually exit
		<-done

		// Write timeout message to log
		fmt.Fprintf(logFile, "\n\n=== HUNG - KILLED AFTER %v ===\n", m.hangTimeout)

		if m.verbose {
			fmt.Printf("make %s for %s hung and was killed after %v\n", makeTarget, packageName, m.hangTimeout)
		}

		return ErrTestHung
	}
}

func (m *MelangeClient) TestPackage(packageName string, withRepo bool, apkRepo string) error {
	return m.runMakeTarget(packageName, fmt.Sprintf("test/%s", packageName), "", withRepo, apkRepo)
}

// BuildPackage runs the build make target for packageName.
// If makeTargetPrefix is empty the target is the package name alone; otherwise
// it is "<makeTargetPrefix>/<packageName>".
func (m *MelangeClient) BuildPackage(packageName string, withRepo bool, apkRepo string, makeTargetPrefix string) error {
	var target string
	if makeTargetPrefix == "" {
		target = packageName
	} else {
		target = fmt.Sprintf("%s/%s", makeTargetPrefix, packageName)
	}
	return m.runMakeTarget(packageName, target, "build", withRepo, apkRepo)
}
