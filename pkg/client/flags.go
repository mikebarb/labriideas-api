package client

import (
	"os"
	"strings"
)

// isFlagSet reports whether the user requested a given flag via either:
//   - a CLI argument: --<name>  (e.g., --dry-run)
//   - an environment variable: <NAME>=true  (e.g., DRY_RUN=true)
//
// Matching is case-insensitive. Unexported so callers must go through the
// typed wrappers below — this prevents misspelled flag names from silently
// returning false.
func isFlagSet(name, envName string) bool {
	cliFlag := "--" + name
	for _, arg := range os.Args[1:] {
		if strings.EqualFold(arg, cliFlag) {
			return true
		}
	}
	return strings.EqualFold(os.Getenv(envName), "true")
}

// IsDryRun reports whether the user requested a dry run.
// Triggered by --dry-run or DRY_RUN=true.
func IsDryRun() bool {
	return isFlagSet("dry-run", "DRY_RUN")
}

// IsCrawlSkipped reports whether the user wants to skip the initial/final
// server crawl during testing. Triggered by --skip-crawl or SKIP_CRAWL=true.
func IsCrawlSkipped() bool {
	return isFlagSet("skip-crawl", "SKIP_CRAWL")
}
