package healthcheck

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

const schemaVersion = 1
const toolName = "mediahealthcheck"

// mediaKind classifies a directory entry by extension.
type mediaKind int

const (
	kindWAV mediaKind = iota
	kindY4M
	kindOther
)

func classify(name string) mediaKind {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".wav"):
		return kindWAV
	case strings.HasSuffix(lower, ".y4m"):
		return kindY4M
	default:
		return kindOther
	}
}

// groupEntry is one on-disk media file waiting to be paired.
type groupEntry struct {
	path string
	name string
	kind mediaKind
	info os.FileInfo
}

// Run walks the configured root read-only, pairs WAV and Y4M files by
// basename, streams each through the bounded scanners, applies the sync
// policy, and assembles the final report. The data root of the running
// service is never opened for writing and no file is imported anywhere.
func Run(options Options) (*Report, error) {
	if options.MemoryBudgetBytes <= 0 {
		options.MemoryBudgetBytes = int64(minBudgetBytes)
	}
	if options.Policy.MinConfidence == 0 && options.Policy.MaxOffsetMS == 0 {
		options.Policy = Policy{
			MinConfidence: domain.DefaultMinConf,
			MaxOffsetMS:   domain.DefaultMaxOffsetMS,
		}
	}

	report := &Report{
		SchemaVersion: schemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Tool:          toolName,
		Root:          options.Root,
		Recursive:     options.Recursive,
		MemoryBudget:  options.MemoryBudgetBytes,
		Policy:        options.Policy,
		Pairs:         []PairReport{},
	}

	directories, err := collectDirectories(options)
	if err != nil {
		return nil, err
	}
	report.Totals.DirectoriesScanned = len(directories)

	for _, dir := range directories {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// An unreadable directory is reported but does not abort the batch.
			report.Pairs = append(report.Pairs, PairReport{
				Key:       dir,
				Directory: dir,
				Verdict:   VerdictIndeterminate,
				Sync:      SyncReport{Status: SyncSkippedChanged},
				Findings: []Finding{{
					Code:     "directory_unreadable",
					Severity: "warn",
					Message:  err.Error(),
				}},
			})
			continue
		}
		groups := pairEntries(dir, entries, report)
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			group := groups[key]
			report.Pairs = append(report.Pairs, scanGroup(dir, key, group, options))
		}
	}

	computeTotals(report)
	return report, nil
}

func collectDirectories(options Options) ([]string, error) {
	info, err := os.Stat(options.Root)
	if err != nil {
		return nil, fmt.Errorf("scan root is not accessible: %w", err)
	}
	if !info.IsDir() {
		return nil, reject("invalid_root", "scan root %q is not a directory", options.Root)
	}
	directories := []string{options.Root}
	if options.Recursive {
		err := filepath.WalkDir(options.Root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && path != options.Root {
				directories = append(directories, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk scan root: %w", err)
		}
	}
	return directories, nil
}

// pairEntries groups media files by lowercase basename without extension and
// records collisions (two WAV files sharing a key) as findings.
func pairEntries(dir string, entries []os.DirEntry, report *Report) map[string][]groupEntry {
	groups := map[string][]groupEntry{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		kind := classify(entry.Name())
		if kind == kindOther {
			continue
		}
		info, err := entry.Info()
	if err != nil {
			report.Pairs = append(report.Pairs, PairReport{
				Key:       filepath.Join(dir, entry.Name()),
				Directory: dir,
				Verdict:   VerdictIndeterminate,
				Sync:      SyncReport{Status: SyncSkippedChanged},
				Findings: []Finding{{
					Code:     "stat_failed",
					Severity: "warn",
					Message:  err.Error(),
				}},
			})
			continue
		}
		key := pairingKey(entry.Name())
		groups[key] = append(groups[key], groupEntry{
			path: filepath.Join(dir, entry.Name()),
			name: entry.Name(),
			kind: kind,
			info: info,
		})
	}
	return groups
}

func pairingKey(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
}

// scanGroup handles one basename group: collision rows, unpaired rows, and full
// WAV+Y4M pairs. Every scan is bracketed by stat and optional digest checks so
// a file that changes during the run can never produce a confident conclusion.
func scanGroup(dir, key string, group []groupEntry, options Options) PairReport {
	pair := PairReport{Key: key, Directory: dir, Findings: []Finding{}}

	var wavs, y4ms []groupEntry
	for _, entry := range group {
		switch entry.kind {
		case kindWAV:
			wavs = append(wavs, entry)
		case kindY4M:
			y4ms = append(y4ms, entry)
		}
	}

	if len(wavs) > 1 || len(y4ms) > 1 {
		pair.Verdict = VerdictIndeterminate
		pair.Sync = SyncReport{Status: SyncSkippedUnpaired}
		names := make([]string, 0, len(group))
		for _, entry := range group {
			names = append(names, entry.name)
		}
		pair.Findings = append(pair.Findings, Finding{
			Code:     "pairing_collision",
			Severity: "reject",
			Message:  fmt.Sprintf("basename %q resolves to multiple files: %s; a take needs exactly one WAV and one Y4M", key, strings.Join(names, ", ")),
		})
		return pair
	}
	if len(wavs) == 0 || len(y4ms) == 0 {
		pair.Verdict = VerdictReject
		pair.Sync = SyncReport{Status: SyncSkippedUnpaired}
		pair.WithinRequestBudget = true // request limits cannot apply to an incomplete pair
		missing := "Y4M"
		if len(wavs) == 0 {
			missing = "WAV"
		}
		pair.Findings = append(pair.Findings, Finding{
			Code:     "unpaired_media",
			Severity: "reject",
			Message:  fmt.Sprintf("no matching %s file found for basename %q", missing, key),
		})
		// Still scan the present file so its descriptor appears in the report.
		if len(wavs) == 1 {
			audio, _ := scanOneWAV(wavs[0], options, &pair)
			pair.Audio = audio
		}
		if len(y4ms) == 1 {
			video, _ := scanOneY4M(y4ms[0], options, &pair)
			pair.Video = video
		}
		return pair
	}

	audioEntry, videoEntry := wavs[0], y4ms[0]
	audio, audioChanged := scanOneWAV(audioEntry, options, &pair)
	video, videoChanged := scanOneY4M(videoEntry, options, &pair)
	pair.Audio = audio
	pair.Video = video
	pair.ChangedDuringScan = audioChanged || videoChanged

	pair.RequestBytesEst = estimateBase64Request(audioEntry.info.Size(), videoEntry.info.Size())
	pair.WithinRequestBudget = pair.RequestBytesEst <= int64(domain.MaxRequestBytes)
	if !pair.WithinRequestBudget {
		pair.Findings = append(pair.Findings, Finding{
			Code:     "request_too_large",
			Severity: "reject",
			Message: sprintf("combined base64 request would be about %d bytes and exceeds the %d byte request limit",
				pair.RequestBytesEst, domain.MaxRequestBytes),
		})
	}

	switch {
	case pair.ChangedDuringScan:
		pair.Verdict = VerdictIndeterminate
		pair.Sync = SyncReport{
			Status:       SyncSkippedChanged,
			ErrorCode:    "file_changed_during_scan",
			ErrorMessage: "one or both files changed while being read; the report cannot assert a sync relationship for stable bytes",
		}
	case audio != nil && video != nil && audio.Admittable && video.Admittable && pair.WithinRequestBudget:
		pair.Verdict = VerdictAccept
		pair.Sync = evaluateSync(audio, video, options.Policy)
	default:
		pair.Verdict = VerdictReject
		pair.Sync = SyncReport{Status: SyncSkippedNotAdmittable}
	}
	return pair
}

func scanOneWAV(entry groupEntry, options Options, pair *PairReport) (*AudioReport, bool) {
	before := entry.info
	audio, err := scanWAVWithHook(entry.path, options.MemoryBudgetBytes, options.scanHook)
	changed := verifyUnchanged(entry.path, before, audioDigest(audio), options, pair, entry.name)
	if err != nil && audio == nil {
		audio = &AudioReport{
			Snapshot:   snapshotInfo(entry.path, before, ""),
			Admittable: false,
			Findings: []Finding{{
				Code:     "read_failed",
				Severity: "reject",
				Message:  err.Error(),
			}},
		}
	}
	return audio, changed
}

func scanOneY4M(entry groupEntry, options Options, pair *PairReport) (*VideoReport, bool) {
	before := entry.info
	video, err := scanY4MWithHook(entry.path, options.MemoryBudgetBytes, options.scanHook)
	changed := verifyUnchanged(entry.path, before, videoDigest(video), options, pair, entry.name)
	if err != nil && video == nil {
		video = &VideoReport{
			Snapshot:   snapshotInfo(entry.path, before, ""),
			Admittable: false,
			Findings: []Finding{{
				Code:     "read_failed",
				Severity: "reject",
				Message:  err.Error(),
			}},
		}
	}
	return video, changed
}

func audioDigest(audio *AudioReport) string {
	if audio == nil {
		return ""
	}
	return audio.Snapshot.DigestSHA256
}

func videoDigest(video *VideoReport) string {
	if video == nil {
		return ""
	}
	return video.Snapshot.DigestSHA256
}

// verifyUnchanged brackets the scan. A stat delta (size, mode, mtime) always
// invalidates the result; with VerifyContent a second full-stream digest also
// catches same-size rewrites.
func verifyUnchanged(path string, before os.FileInfo, firstDigest string, options Options, pair *PairReport, label string) bool {
	after, err := os.Stat(path)
	changed := false
	if err != nil {
		changed = true
		pair.Findings = append(pair.Findings, Finding{
			Code:     "file_vanished",
			Severity: "reject",
			Message:  sprintf("%s is no longer stat-able after the scan: %v", label, err),
		})
	} else if after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		changed = true
		pair.Findings = append(pair.Findings, Finding{
			Code:     "file_changed_during_scan",
			Severity: "reject",
			Message:  sprintf("%s changed during the scan (stat before: size=%d mtime=%s; after: size=%d mtime=%s)",
				label, before.Size(), before.ModTime().UTC().Format(time.RFC3339Nano),
				after.Size(), after.ModTime().UTC().Format(time.RFC3339Nano)),
		})
	}
	if options.VerifyContent && !changed && firstDigest != "" {
		secondDigest, err := hashFile(path, int(options.MemoryBudgetBytes))
		if err != nil {
			changed = true
			pair.Findings = append(pair.Findings, Finding{
				Code:     "file_vanished",
				Severity: "reject",
				Message:  sprintf("%s could not be re-read for content verification: %v", label, err),
			})
		} else if secondDigest != firstDigest {
			changed = true
			pair.Findings = append(pair.Findings, Finding{
				Code:     "content_changed_during_scan",
				Severity: "reject",
				Message:  sprintf("%s content digest changed between the scan and verification", label),
			})
		}
	}
	return changed
}

// evaluateSync mirrors service.Analyze's decision gates on the measurements
// produced by the streaming detectors.
func evaluateSync(audio *AudioReport, video *VideoReport, policy Policy) SyncReport {
	if !audio.OnsetFound || !video.FlashFound {
		sync := SyncReport{Status: SyncDetectionFailed, ErrorCode: SyncDetectionFailed}
		details := []string{}
		if !audio.OnsetFound {
			details = append(details, lastInfoMessage(audio.Findings, "audio onset was not found"))
		}
		if !video.FlashFound {
			details = append(details, lastInfoMessage(video.Findings, "picture flash was not found"))
		}
		sync.ErrorMessage = strings.Join(details, "; ")
		return sync
	}

	audioEvent := audio.OnsetEventMS
	videoEvent := video.FlashEventMS
	offset := audioEvent - videoEvent
	span := maxInt(audioEvent, videoEvent)
	if span < 1 {
		span = 1
	}
	drift := int(math.Round(float64(offset) * 1_000_000 / float64(span*1000)))
	confidence := math.Round(((audio.Prominence+video.Prominence)/2)*1000) / 1000

	sync := SyncReport{
		Status:          SyncReady,
		AudioEventMS:    audioEvent,
		VideoEventMS:    videoEvent,
		OffsetMS:        offset,
		Confidence:      confidence,
		DriftPPM:        drift,
		AudioProminence: audio.Prominence,
		VideoProminence: video.Prominence,
	}
	if confidence < policy.MinConfidence {
		sync.Status = SyncLowConfidence
		sync.ErrorCode = SyncLowConfidence
		sync.ErrorMessage = sprintf("confidence %.3f is below the minimum %.3f", confidence, policy.MinConfidence)
		return sync
	}
	if absInt(offset) > policy.MaxOffsetMS {
		sync.Status = SyncOffsetExceeded
		sync.ErrorCode = SyncOffsetExceeded
		sync.ErrorMessage = sprintf("offset %d ms exceeds the analysis limit of %d ms", absInt(offset), policy.MaxOffsetMS)
		return sync
	}
	return sync
}

func lastInfoMessage(findings []Finding, fallback string) string {
	for i := len(findings) - 1; i >= 0; i-- {
		if findings[i].Severity == "info" {
			return findings[i].Message
		}
	}
	return fallback
}

// estimateBase64Request reproduces the 4/3 overhead of strict base64 encoding
// used by the HTTP admission route, ignoring envelope JSON overhead.
func estimateBase64Request(audioBytes, videoBytes int64) int64 {
	encoded := func(n int64) int64 { return ((n + 2) / 3) * 4 }
	return encoded(audioBytes) + encoded(videoBytes)
}

func computeTotals(report *Report) {
	for _, pair := range report.Pairs {
		if pair.Audio != nil {
			report.Totals.WAVFiles++
		}
		if pair.Video != nil {
			report.Totals.Y4MFiles++
		}
		if pair.Audio != nil && pair.Video != nil {
			report.Totals.Paired++
		}
		switch pair.Verdict {
		case VerdictAccept:
			report.Totals.Admittable++
		case VerdictReject:
			report.Totals.NotAdmittable++
		case VerdictIndeterminate:
			report.Totals.Indeterminate++
		}
		if pair.Sync.Status == SyncReady {
			report.Totals.SyncReady++
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
