// Command mediahealthcheck is a reusable, read-only local media inventory and
// synchronization probe. It pairs WAV and Y4M files in an operator-chosen
// directory, determines whether the current sync-conformer service would
// accept each pair, and describes the analyzable audio/picture relationship.
//
// The command never writes into the scanned tree and never imports media into
// the service data directory: all files are opened read-only through bounded
// streaming buffers, so a batch larger than the memory budget is still
// processed. A file that changes while it is being read invalidates the
// affected pair instead of producing a confident but stale conclusion.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/healthcheck"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "mediahealthcheck: %v\n", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("mediahealthcheck", flag.ContinueOnError)
	root := flags.String("dir", ".", "directory containing WAV and Y4M media to inspect")
	recursive := flags.Bool("recursive", false, "descend into subdirectories while pairing media")
	format := flags.String("format", "text", "report format: text or json")
	reportPath := flags.String("report", "", "write the report to this path instead of stdout")
	memoryMiB := flags.Int("memory-budget-mib", 16, "maximum bytes used per streaming read buffer, in MiB; the scan never loads whole files")
	verifyContent := flags.Bool("verify-content", false, "re-hash every file after scanning to detect same-size rewrites as well as stat changes")
	minConfidence := flags.Float64("min-confidence", domain.DefaultMinConf, "sync policy: minimum confidence for a ready analysis")
	maxOffsetMS := flags.Int("max-offset-ms", domain.DefaultMaxOffsetMS, "sync policy: maximum absolute offset in milliseconds")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("format must be text or json, got %q", *format)
	}
	if *memoryMiB < 1 {
		return fmt.Errorf("memory-budget-mib must be at least 1")
	}
	if *minConfidence < 0 || *minConfidence > 1 {
		return fmt.Errorf("min-confidence must be between 0 and 1")
	}
	if *maxOffsetMS < 1 || *maxOffsetMS > 10000 {
		return fmt.Errorf("max-offset-ms must be between 1 and 10000")
	}

	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return fmt.Errorf("resolve scan directory: %w", err)
	}

	report, err := healthcheck.Run(healthcheck.Options{
		Root:              absoluteRoot,
		Recursive:         *recursive,
		MemoryBudgetBytes: int64(*memoryMiB) * 1024 * 1024,
		VerifyContent:     *verifyContent,
		Policy: healthcheck.Policy{
			MinConfidence: *minConfidence,
			MaxOffsetMS:   *maxOffsetMS,
		},
	})
	if err != nil {
		return err
	}

	output, closeOutput, err := openOutput(*reportPath)
	if err != nil {
		return err
	}
	defer closeOutput()

	if *format == "json" {
		err = healthcheck.WriteJSON(output, report)
	} else {
		err = healthcheck.WriteText(output, report)
	}
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	// Exit status gives scripts a deterministic signal without hiding detail:
	// 0 when every group is admittable and stable, 1 when any pair is rejected
	// or could not be trusted.
	if report.Totals.NotAdmittable > 0 || report.Totals.Indeterminate > 0 {
		os.Exit(1)
	}
	return nil
}

// openOutput returns stdout or a newly created/truncated file. The report path
// is deliberately independent of the scanned tree; the scanned media itself is
// never opened for writing.
func openOutput(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("open report file: %w", err)
	}
	return file, func() { _ = file.Close() }, nil
}
