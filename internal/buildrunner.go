// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright 2025 Chainguard, Inc.

package internal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

type BuildRegressionRunner struct {
	buildDep       string
	apkRepo        string
	repoPath       string
	concurrency    int
	verbose        bool
	logDir         string
	hangTimeout    time.Duration
	markdownOutput bool
	melange        *MelangeClient
	completedTests int64
	totalTests     int64
	startTime      time.Time
}

func NewBuildRegressionRunner(buildDep, apkRepo, repoPath string, concurrency int, verbose bool, hangTimeout time.Duration, markdownOutput bool) *BuildRegressionRunner {
	// Create log directory with timestamp to avoid collisions between runs
	timestamp := time.Now().Format("20060102-150405")
	logDir := filepath.Join("logs", fmt.Sprintf("build-regression-%s-%s", buildDep, timestamp))

	// Default to 2 hours if no timeout specified; builds take significantly
	// longer than tests so we use a higher default than the test runner.
	if hangTimeout == 0 {
		hangTimeout = 2 * time.Hour
	}

	return &BuildRegressionRunner{
		buildDep:       buildDep,
		apkRepo:        apkRepo,
		repoPath:       repoPath,
		concurrency:    concurrency,
		verbose:        verbose,
		logDir:         logDir,
		hangTimeout:    hangTimeout,
		markdownOutput: markdownOutput,
		melange:        NewMelangeClient(repoPath, verbose, logDir, hangTimeout),
	}
}

func NewBuildRegressionRunnerFromPackageList(packages []string, apkRepo, repoPath string, concurrency int, verbose bool, hangTimeout time.Duration, markdownOutput bool) *BuildRegressionRunner {
	// Create log directory with timestamp to avoid collisions between runs
	timestamp := time.Now().Format("20060102-150405")
	logDir := filepath.Join("logs", fmt.Sprintf("build-regression-list-%s", timestamp))

	// Default to 2 hours if no timeout specified; builds take significantly
	// longer than tests so we use a higher default than the test runner.
	if hangTimeout == 0 {
		hangTimeout = 2 * time.Hour
	}

	return &BuildRegressionRunner{
		buildDep:       fmt.Sprintf("%d packages from file", len(packages)),
		apkRepo:        apkRepo,
		repoPath:       repoPath,
		concurrency:    concurrency,
		verbose:        verbose,
		logDir:         logDir,
		hangTimeout:    hangTimeout,
		markdownOutput: markdownOutput,
		melange:        NewMelangeClient(repoPath, verbose, logDir, hangTimeout),
	}
}

func (r *BuildRegressionRunner) updateProgress() {
	// Check current value before incrementing
	current := atomic.LoadInt64(&r.completedTests)
	total := r.totalTests

	if current >= total {
		return // Already at or past completion
	}

	completed := atomic.AddInt64(&r.completedTests, 1)

	if r.verbose {
		return // Don't show progress in verbose mode
	}

	// Calculate progress percentage
	progress := float64(completed) / float64(total) * 100

	// Calculate elapsed time and estimate remaining time
	elapsed := time.Since(r.startTime)
	var eta time.Duration
	if completed > 0 {
		avgTimePerTest := elapsed / time.Duration(completed)
		eta = avgTimePerTest * time.Duration(total-completed)
	}

	// Format the progress update
	if eta > 0 {
		fmt.Printf("\rProgress: %d/%d (%.1f%%) - ETA: %v", completed, total, progress, eta.Round(time.Second))
	} else {
		fmt.Printf("\rProgress: %d/%d (%.1f%%)", completed, total, progress)
	}

	// Print newline when complete
	if completed == total {
		fmt.Println()
	}
}

// Run discovers packages that build-depend on r.buildDep and rebuilds them.
func (r *BuildRegressionRunner) Run() error {
	// Create log directory
	if err := os.MkdirAll(r.logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %s: %w", r.logDir, err)
	}

	// Find all packages in the repository that list r.buildDep as a build dependency
	buildDeps, err := GetBuildDependents(r.repoPath, r.buildDep, r.verbose)
	if err != nil {
		return fmt.Errorf("failed to find build dependents: %w", err)
	}

	if len(buildDeps) == 0 {
		fmt.Printf("No packages found with build dependency on: %s\n", r.buildDep)
		return nil
	}

	fmt.Printf("Rebuilding %d packages that build-depend on %s, concurrency %d\n", len(buildDeps), r.buildDep, r.concurrency)
	fmt.Printf("Logs will be saved to: %s\n", r.logDir)

	return r.runPackages(buildDeps)
}

// RunFromPackageList rebuilds a pre-defined list of packages.
func (r *BuildRegressionRunner) RunFromPackageList(packages []string) error {
	// Create log directory
	if err := os.MkdirAll(r.logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %s: %w", r.logDir, err)
	}

	if len(packages) == 0 {
		fmt.Println("No packages provided")
		return nil
	}

	fmt.Printf("Rebuilding %d packages with concurrency %d\n", len(packages), r.concurrency)
	fmt.Printf("Logs will be saved to: %s\n", r.logDir)

	return r.runPackages(packages)
}

func (r *BuildRegressionRunner) runPackages(packages []string) error {
	// Initialize progress tracking
	r.totalTests = int64(len(packages))
	r.startTime = time.Now()

	results := make(chan TestResult, len(packages)*2)
	ctx := context.Background()
	sem := semaphore.NewWeighted(int64(r.concurrency))
	var wg sync.WaitGroup

	for _, pkg := range packages {
		wg.Add(1)
		go func(packageName string) {
			defer wg.Done()
			sem.Acquire(ctx, 1)
			defer sem.Release(1)

			// First attempt: build with the new APK repository
			err := r.melange.BuildPackage(packageName, true, r.apkRepo)

			withRepoResult := TestResult{
				Package:  packageName,
				WithRepo: true,
				Success:  err == nil,
				Error:    err,
				Hung:     errors.Is(err, ErrTestHung),
				Skipped:  errors.Is(err, ErrPackageYAMLNotFound),
			}
			results <- withRepoResult

			// Only attempt without the repo if the build failed and wasn't skipped or hung.
			// A hung build indicates a pre-existing infrastructure problem, not a regression.
			if !withRepoResult.Success && !withRepoResult.Skipped && !withRepoResult.Hung {
				err := r.melange.BuildPackage(packageName, false, r.apkRepo)

				// Skip if YAML file not found (shouldn't happen since we already checked, but for safety)
				if errors.Is(err, ErrPackageYAMLNotFound) {
					r.updateProgress()
					return
				}

				results <- TestResult{
					Package:  packageName,
					WithRepo: false,
					Success:  err == nil,
					Error:    err,
					Hung:     errors.Is(err, ErrTestHung),
					Skipped:  errors.Is(err, ErrPackageYAMLNotFound),
				}
			}

			// Update progress after completing all build attempts for this package
			r.updateProgress()
		}(pkg)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	return r.analyzeResults(results, len(packages))
}

func (r *BuildRegressionRunner) analyzeResults(results chan TestResult, expectedPackages int) error {
	packageResults := make(map[string]map[bool]TestResult)

	for result := range results {
		if packageResults[result.Package] == nil {
			packageResults[result.Package] = make(map[bool]TestResult)
		}
		packageResults[result.Package][result.WithRepo] = result
	}

	var regressions []string
	var hungTests []string
	var successfulPackages []string
	var failedPackages []string
	var skippedPackages []string
	var successCount, failureCount, skippedCount int

	fmt.Println("\n=== Build Results ===")
	for pkg, results := range packageResults {
		withRepoResult, hasWithRepo := results[true]
		withoutRepoResult, hasWithoutRepo := results[false]

		if !hasWithRepo {
			fmt.Printf("⚠️  %s: Incomplete results\n", pkg)
			continue
		}

		// Check for skipped packages first
		if withRepoResult.Skipped {
			skippedCount++
			skippedPackages = append(skippedPackages, pkg)
			if r.verbose {
				fmt.Printf("⏭️  %s: SKIPPED (YAML file not found)\n", pkg)
			}
			continue
		}

		// Check for hung builds
		if withRepoResult.Hung {
			hungTests = append(hungTests, fmt.Sprintf("%s (with repo)", pkg))
			fmt.Printf("⏰ %s: HUNG (with repo - killed after %v)\n", pkg, r.hangTimeout)
			if hasWithoutRepo && withoutRepoResult.Hung {
				hungTests = append(hungTests, fmt.Sprintf("%s (without repo)", pkg))
				fmt.Printf("⏰ %s: HUNG (without repo - killed after %v)\n", pkg, r.hangTimeout)
			}
			continue
		}
		if hasWithoutRepo && withoutRepoResult.Hung {
			hungTests = append(hungTests, fmt.Sprintf("%s (without repo)", pkg))
			fmt.Printf("⏰ %s: HUNG (without repo - killed after %v)\n", pkg, r.hangTimeout)
			continue
		}

		// If the build with the repo passed, we didn't run the without-repo build
		if withRepoResult.Success && !hasWithoutRepo {
			successCount++
			successfulPackages = append(successfulPackages, pkg)
			if r.verbose {
				fmt.Printf("✅ %s: BUILD PASS\n", pkg)
			}
		} else if !withRepoResult.Success && hasWithoutRepo {
			// Both builds were run because the with-repo build failed
			if withoutRepoResult.Success {
				// Fails with the new repo but passes without it: build regression
				regressions = append(regressions, pkg)
				fmt.Printf("🔴 %s: BUILD REGRESSION (fails with new repo, passes without)\n", pkg)
			} else {
				// Fails in both scenarios: pre-existing build failure, not a regression
				failureCount++
				failedPackages = append(failedPackages, pkg)
				if r.verbose {
					fmt.Printf("❌ %s: BUILD FAIL (both scenarios)\n", pkg)
				}
			}
		}
	}

	// Generate result files
	r.writeResultFiles(successfulPackages, failedPackages, regressions, hungTests, skippedPackages)

	if r.markdownOutput {
		r.printMarkdownSummary(expectedPackages, skippedCount, len(packageResults)-skippedCount, len(regressions), len(hungTests), successCount, failureCount, regressions, hungTests)
	} else {
		fmt.Printf("\n=== Summary ===\n")
		fmt.Printf("Total packages: %d\n", expectedPackages)
		fmt.Printf("Packages skipped (no YAML): %d\n", skippedCount)
		fmt.Printf("Packages rebuilt: %d\n", len(packageResults)-skippedCount)
		fmt.Printf("Build regressions: %d\n", len(regressions))
		fmt.Printf("Hung builds: %d\n", len(hungTests))
		fmt.Printf("Successful builds: %d\n", successCount)
		fmt.Printf("Failed builds (pre-existing): %d\n", failureCount)
	}

	if !r.markdownOutput {
		if len(hungTests) > 0 {
			fmt.Printf("\nBuilds that hung (killed after %v):\n", r.hangTimeout)
			for _, test := range hungTests {
				fmt.Printf("  - %s\n", test)
			}
		}
		if len(regressions) > 0 {
			fmt.Printf("\nPackages with build regressions:\n")
			for _, pkg := range regressions {
				fmt.Printf("  - %s\n", pkg)
			}
		}
	}

	if len(regressions) > 0 {
		return fmt.Errorf("found %d build regressions", len(regressions))
	}
	if len(hungTests) > 0 {
		return fmt.Errorf("found %d hung builds", len(hungTests))
	}
	return nil
}

func (r *BuildRegressionRunner) printMarkdownSummary(totalPackages, skippedCount, rebuiltCount, regressionsCount, hungCount, successCount, failureCount int, regressions, hungTests []string) {
	fmt.Printf("\n## APK Build Regression Summary\n\n")
	fmt.Printf("**Build dependency:** %s  \n", r.buildDep)
	fmt.Printf("**APK Repository:** %s  \n", r.apkRepo)
	fmt.Printf("**Duration:** %v  \n\n", time.Since(r.startTime).Round(time.Second))

	fmt.Printf("### Build Results\n\n")
	fmt.Printf("| Metric | Count |\n")
	fmt.Printf("|--------|-------|\n")
	fmt.Printf("| Total packages found | %d |\n", totalPackages)
	fmt.Printf("| Packages skipped (no YAML) | %d |\n", skippedCount)
	fmt.Printf("| Packages rebuilt | %d |\n", rebuiltCount)
	fmt.Printf("| **Build regressions** | **%d** |\n", regressionsCount)
	fmt.Printf("| Hung builds | %d |\n", hungCount)
	fmt.Printf("| Successful builds | %d |\n", successCount)
	fmt.Printf("| Pre-existing build failures | %d |\n", failureCount)

	if regressionsCount > 0 {
		fmt.Printf("\n### 🔴 Packages with Build Regressions\n\n")
		fmt.Printf("These packages **fail to build with the new repository** but **succeed without it**:\n\n")
		for _, pkg := range regressions {
			fmt.Printf("- `%s`\n", pkg)
		}
	}

	if hungCount > 0 {
		fmt.Printf("\n### ⏰ Builds That Hung\n\n")
		fmt.Printf("The following builds were killed after %v timeout:\n\n", r.hangTimeout)
		for _, test := range hungTests {
			fmt.Printf("- `%s`\n", test)
		}
	}

	if regressionsCount == 0 && hungCount == 0 {
		fmt.Printf("\n### ✅ All Builds Passed\n\n")
		fmt.Printf("No build regressions detected. All packages either built successfully with the new repository or failed consistently in both scenarios.\n")
	}

	fmt.Printf("\n---\n")
	fmt.Printf("*Generated by apk-regression-test-runner*\n")
}

func (r *BuildRegressionRunner) writeResultFiles(successful, failed, regressions, hung, skipped []string) {
	files := map[string][]string{
		"successful.txt":  successful,
		"failed.txt":      failed,
		"regressions.txt": regressions,
		"hung.txt":        hung,
		"skipped.txt":     skipped,
	}

	for filename, packages := range files {
		filePath := filepath.Join(r.logDir, filename)
		content := strings.Join(packages, "\n")
		if content != "" {
			content += "\n"
		}
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			fmt.Printf("Warning: failed to write %s: %v\n", filename, err)
		}
	}
}
