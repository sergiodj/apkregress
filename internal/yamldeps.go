// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Copyright 2025 Chainguard, Inc.

package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// melangeYAML captures only the fields from a melange package YAML that are
// needed to determine build dependencies.
type melangeYAML struct {
	Package struct {
		Name string `yaml:"name"`
	} `yaml:"package"`
	Environment struct {
		Contents struct {
			Packages []string `yaml:"packages"`
		} `yaml:"contents"`
	} `yaml:"environment"`
}

// GetBuildDependents scans all *.yaml files in repoPath and returns the
// origin names of packages that list pkgName as a direct build dependency.
// Packages that also have a build dependency matching any regexp in excludePatterns
// are omitted from the results.
func GetBuildDependents(repoPath, pkgName string, excludePatterns []string, verbose bool) ([]string, error) {
	// Compile exclude patterns up front so we catch invalid regexps before scanning
	excludeRegexps := make([]*regexp.Regexp, 0, len(excludePatterns))
	for _, pattern := range excludePatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid --exclude pattern %q: %w", pattern, err)
		}
		excludeRegexps = append(excludeRegexps, re)
	}

	// Collect all melange YAML files in the repository
	entries, err := filepath.Glob(filepath.Join(repoPath, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("failed to scan YAML files in %s: %w", repoPath, err)
	}

	// Use a set to deduplicate origins (a package may produce multiple subpackages,
	// each with its own YAML, but they all share the same origin name)
	seen := make(map[string]bool)
	var results []string

	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			if verbose {
				fmt.Printf("Warning: failed to read %s: %v\n", path, err)
			}
			continue
		}

		var pkg melangeYAML
		if err := yaml.Unmarshal(data, &pkg); err != nil {
			if verbose {
				fmt.Printf("Warning: failed to parse %s: %v\n", path, err)
			}
			continue
		}

		origin := pkg.Package.Name
		if origin == "" || seen[origin] {
			continue
		}

		// Check whether pkgName appears in the build environment's package list
		if !slices.Contains(pkg.Environment.Contents.Packages, pkgName) {
			continue
		}

		// Skip packages that have a build dependency matching any exclude pattern
		excluded := false
		for _, re := range excludeRegexps {
			for _, dep := range pkg.Environment.Contents.Packages {
				if re.MatchString(dep) {
					if verbose {
						fmt.Printf("Excluding %s: build-depends on %s (matches %s)\n", origin, dep, re)
					}
					excluded = true
					break
				}
			}
			if excluded {
				break
			}
		}
		if excluded {
			continue
		}

		seen[origin] = true
		results = append(results, origin)
	}

	sort.Strings(results)

	if verbose {
		fmt.Printf("Found %d packages with build dependency on %s\n", len(results), pkgName)
	}

	return results, nil
}
