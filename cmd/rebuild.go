// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright 2025 Chainguard, Inc.

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chainguard-dev/apkregress/internal"
	"github.com/spf13/cobra"
)

var (
	rebuildMakeTargetPrefix string
	rebuildHangTimeout      time.Duration
	rebuildMarkdownOutput   bool
	rebuildVerbose          bool
	rebuildConcurrency      int
	rebuildPackageName      string
	rebuildPackageFile      string
)

var rebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild packages that build-depend on a given package",
	Long: `Scans the local package repository YAML files to find packages that list
the given package as a direct build dependency, then attempts to rebuild each
one against the provided APK repository.

A build regression is detected when a package fails to build with the new
repository but succeeds without it.

Use --package build-base to cover packages that transitively depend on GCC
(or another compiler), since build-base is the common build meta-package that
pulls in the compiler toolchain.`,
	RunE: runRebuild,
}

func init() {
	rebuildCmd.Flags().StringVarP(&rebuildPackageName, "package", "p", "", "Build dependency to search for (e.g. build-base)")
	rebuildCmd.Flags().StringVarP(&rebuildPackageFile, "package-file", "f", "", "File containing package names to rebuild (one per line)")
	rebuildCmd.Flags().StringVarP(&apkRepo, "repo", "r", "", "APK repository URL containing the new package version (required)")
	rebuildCmd.Flags().StringVarP(&repoPath, "repo-path", "w", "", "Path to the local package repository with YAML files (required)")
	rebuildCmd.Flags().IntVarP(&rebuildConcurrency, "concurrency", "c", 4, "Number of concurrent build jobs")
	rebuildCmd.Flags().BoolVarP(&rebuildVerbose, "verbose", "v", false, "Enable verbose output")
	rebuildCmd.Flags().DurationVar(&rebuildHangTimeout, "hang-timeout", 2*time.Hour, "Timeout for hung builds")
	rebuildCmd.Flags().BoolVarP(&rebuildMarkdownOutput, "markdown", "m", false, "Output summary in markdown format")
	rebuildCmd.Flags().StringVar(&rebuildMakeTargetPrefix, "make-target-prefix", "", `Prefix for the make build target. Empty means "make <package>";
"package" means "make package/<package>"`)

	rebuildCmd.MarkFlagRequired("repo")
	rebuildCmd.MarkFlagRequired("repo-path")

	rootCmd.AddCommand(rebuildCmd)
}

func runRebuild(cmd *cobra.Command, args []string) error {
	if rebuildPackageName == "" && rebuildPackageFile == "" {
		return fmt.Errorf("either --package or --package-file must be specified")
	}
	if rebuildPackageName != "" && rebuildPackageFile != "" {
		return fmt.Errorf("cannot specify both --package and --package-file")
	}

	if !filepath.IsAbs(repoPath) {
		absPath, err := filepath.Abs(repoPath)
		if err != nil {
			return fmt.Errorf("failed to resolve repository path: %w", err)
		}
		repoPath = absPath
	}

	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return fmt.Errorf("repository path does not exist: %s", repoPath)
	}

	if rebuildPackageFile != "" {
		packages, err := readPackageFile(rebuildPackageFile)
		if err != nil {
			return fmt.Errorf("failed to read package file: %w", err)
		}
		runner := internal.NewBuildRegressionRunnerFromPackageList(
			packages, apkRepo, repoPath, rebuildMakeTargetPrefix,
			rebuildConcurrency, rebuildVerbose, rebuildHangTimeout, rebuildMarkdownOutput,
		)
		return runner.RunFromPackageList(packages)
	}

	runner := internal.NewBuildRegressionRunner(
		rebuildPackageName, apkRepo, repoPath, rebuildMakeTargetPrefix,
		rebuildConcurrency, rebuildVerbose, rebuildHangTimeout, rebuildMarkdownOutput,
	)
	return runner.Run()
}
