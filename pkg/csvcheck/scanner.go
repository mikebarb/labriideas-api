// Package csvcheck validates that a CSV file is in a format Go's encoding/csv
// can read correctly. It scans the **entire** file for any encoding artifacts
// that would be fixed by re‑exporting with Excel's "CSV (Comma Delimited)" option.
package csvcheck

import (
	"fmt"
	"os"
	"strings"
)

// Result describes the outcome of a CSV format check.
type Result struct {
	OK       bool
	Problems []string // Human-readable problems found
	Advice   string   // What the user should do
}

// suspiciousSubstrings are case‑insensitive patterns that indicate a non‑plain
// CSV export. Any hit means the file was NOT exported as a true "CSV (Comma
// Delimited)" file and needs to be re‑exported.
var suspiciousSubstrings = []string{
	// Microsoft's "CSV UTF-8 (Comma Delimited)" quoted‑printable
	"=?utf-8?q?", // quoted‑printable
	"=?utf-8?b?", // base64 variant
	// Older / European locale variants
	"=?iso-8859-1?q?",
	"=?iso-8859-1?b?",
	"=?iso-8859-15?q?",
	"=?iso-8859-15?b?",
	"=?windows-1252?q?",
	"=?windows-1252?b?",
	// Rare but possible
	"=?utf-16?q?",
	"=?utf-16?b?",
}

// Check opens the file at path, reads it entirely into memory, and scans for
// suspicious encoding markers.
func Check(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}

	content := string(data)
	lower := strings.ToLower(content)

	res := &Result{OK: true}

	// Check for UTF-16 BOM at the start (both LE and BE)
	if len(data) >= 2 {
		if (data[0] == 0xff && data[1] == 0xfe) ||
			(data[0] == 0xfe && data[1] == 0xff) {
			res.OK = false
			res.Problems = append(res.Problems,
				"file starts with a UTF-16 byte‑order mark (BOM). "+
					"This is not a valid UTF-8 CSV and will be read as garbage.")
		}
	}

	// Scan for MIME encoded‑word patterns (case‑insensitive)
	for _, sig := range suspiciousSubstrings {
		if strings.Contains(lower, sig) {
			res.OK = false
			res.Problems = append(res.Problems,
				fmt.Sprintf("found %q — this is an MIME encoded‑word that "+
					`indicates Excel's "CSV UTF-8 (Comma Delimited)" export.`, sig))
		}
	}

	if !res.OK {
		res.Advice = `This file was NOT saved as a plain "CSV (Comma Delimited)" file.
It contains MIME encoded‑word headers (like =?utf-8?Q?...) that Go's CSV
reader cannot decode. Such files will produce garbled text/data.

To fix:
  1. Open the file in Excel.
  2. File -> Save As.
  3. Choose "CSV (Comma delimited) (*.csv)" — NOT "CSV UTF-8 (Comma Delimited)".
  4. Save and overwrite the file.

Alternative: open in Google Sheets and download as CSV, or use LibreOffice
Calc with charset = UTF-8.
`
	}

	return res, nil
}

// MustCheck runs Check and, if the file is not OK, prints a friendly error
// to stderr and aborts the program. Use this at the top of main().
func MustCheck(path string) {
	res, err := Check(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ CSV check failed: %v\n", err)
		os.Exit(1)
	}
	if res.OK {
		if len(res.Problems) > 0 {
			// Non-fatal warnings (e.g., UTF-8 BOM — handled fine by Go,
			// but worth flagging for other tools)
			for _, p := range res.Problems {
				fmt.Fprintf(os.Stderr, "⚠️  CSV warning: %s\n", p)
			}
		}
		return
	}

	// Fatal — print a clear, actionable error and abort.
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "❌ CSV file format is not supported.")
	fmt.Fprintln(os.Stderr, "")
	for _, p := range res.Problems {
		fmt.Fprintf(os.Stderr, "  • %s\n", p)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, res.Advice)
	fmt.Fprintln(os.Stderr, "")
	os.Exit(1)
}
