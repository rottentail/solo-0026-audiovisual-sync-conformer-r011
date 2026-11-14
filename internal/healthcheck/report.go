package healthcheck

import (
	"encoding/json"
	"io"
	"strings"
)

// WriteJSON emits the deterministic machine-readable report. Field order is
// fixed by the struct declarations in model.go.
func WriteJSON(w io.Writer, report *Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	return nil
}

// WriteText emits a human-readable report suitable for terminal review or an
// archived plain-text artifact.
func WriteText(w io.Writer, report *Report) error {
	var b strings.Builder
	b.WriteString("MEDIA HEALTH CHECK REPORT\n")
	b.WriteString("=========================\n")
	writeRow(&b, "Generated (UTC)", report.GeneratedAt.String())
	writeRow(&b, "Tool", report.Tool)
	writeRow(&b, "Scanned root", report.Root)
	writeRow(&b, "Recursive", boolText(report.Recursive))
	writeRow(&b, "Memory budget (bytes)", itoa(report.MemoryBudget))
	writeRow(&b, "Policy min confidence", ftoa(report.Policy.MinConfidence))
	writeRow(&b, "Policy max offset (ms)", itoa(int64(report.Policy.MaxOffsetMS)))
	b.WriteString("\nSUMMARY\n")
	b.WriteString("-------\n")
	t := report.Totals
	writeRow(&b, "Directories scanned", itoa(int64(t.DirectoriesScanned)))
	writeRow(&b, "WAV files", itoa(int64(t.WAVFiles)))
	writeRow(&b, "Y4M files", itoa(int64(t.Y4MFiles)))
	writeRow(&b, "Paired groups", itoa(int64(t.Paired)))
	writeRow(&b, "Admittable pairs", itoa(int64(t.Admittable)))
	writeRow(&b, "Not admittable pairs", itoa(int64(t.NotAdmittable)))
	writeRow(&b, "Indeterminate groups", itoa(int64(t.Indeterminate)))
	writeRow(&b, "Sync-ready pairs", itoa(int64(t.SyncReady)))

	for _, pair := range report.Pairs {
		b.WriteString("\n----------------------------------------\n")
		b.WriteString("GROUP: ")
		b.WriteString(pair.Key)
		b.WriteString("\n")
		writeRow(&b, "Directory", pair.Directory)
		writeRow(&b, "Verdict", pair.Verdict)
		writeRow(&b, "Changed during scan", boolText(pair.ChangedDuringScan))
		writeRow(&b, "Base64 request estimate (bytes)", itoa(pair.RequestBytesEst))
		writeRow(&b, "Within request budget", boolText(pair.WithinRequestBudget))

		if pair.Audio != nil {
			b.WriteString("  Audio (WAV):\n")
			writeRow(&b, "    Path", pair.Audio.Snapshot.Path)
			writeRow(&b, "    Admittable", boolText(pair.Audio.Admittable))
			writeRow(&b, "    SHA-256", pair.Audio.Snapshot.DigestSHA256)
			writeRow(&b, "    Size (bytes)", itoa(pair.Audio.Snapshot.Size))
			writeRow(&b, "    Sample rate", itoa(int64(pair.Audio.SampleRate)))
			writeRow(&b, "    Channels", itoa(int64(pair.Audio.Channels)))
			writeRow(&b, "    Bit depth", itoa(int64(pair.Audio.BitDepth)))
			writeRow(&b, "    Frames", itoa(int64(pair.Audio.FrameCount)))
			writeRow(&b, "    Duration (ms)", itoa(int64(pair.Audio.DurationMS)))
			writeRow(&b, "    Peak (dBFS)", ftoa(pair.Audio.PeakDBFS))
			writeRow(&b, "    RMS (dBFS)", ftoa(pair.Audio.RMSDBFS))
			writeRow(&b, "    Onset found", boolText(pair.Audio.OnsetFound))
			if pair.Audio.OnsetFound {
				writeRow(&b, "    Onset event (ms)", itoa(int64(pair.Audio.OnsetEventMS)))
				writeRow(&b, "    Onset prominence", ftoa(pair.Audio.Prominence))
			}
			writeFindings(&b, pair.Audio.Findings, "      ")
		}
		if pair.Video != nil {
			b.WriteString("  Video (Y4M):\n")
			writeRow(&b, "    Path", pair.Video.Snapshot.Path)
			writeRow(&b, "    Admittable", boolText(pair.Video.Admittable))
			writeRow(&b, "    SHA-256", pair.Video.Snapshot.DigestSHA256)
			writeRow(&b, "    Size (bytes)", itoa(pair.Video.Snapshot.Size))
			writeRow(&b, "    Geometry", sprintf("%dx%d", pair.Video.Width, pair.Video.Height))
			writeRow(&b, "    Frame rate", sprintf("%d:%d", pair.Video.FPSNum, pair.Video.FPSDen))
			writeRow(&b, "    Chroma", pair.Video.Chroma)
			writeRow(&b, "    Frames", itoa(int64(pair.Video.FrameCount)))
			writeRow(&b, "    Duration (ms)", itoa(int64(pair.Video.DurationMS)))
			writeRow(&b, "    Flash found", boolText(pair.Video.FlashFound))
			if pair.Video.FlashFound {
				writeRow(&b, "    Flash event (ms)", itoa(int64(pair.Video.FlashEventMS)))
				writeRow(&b, "    Flash prominence", ftoa(pair.Video.Prominence))
			}
			writeFindings(&b, pair.Video.Findings, "      ")
		}
		b.WriteString("  Sync analysis:\n")
		writeRow(&b, "    Status", pair.Sync.Status)
		if pair.Sync.Status == SyncReady {
			writeRow(&b, "    Audio event (ms)", itoa(int64(pair.Sync.AudioEventMS)))
			writeRow(&b, "    Video event (ms)", itoa(int64(pair.Sync.VideoEventMS)))
			writeRow(&b, "    Offset (ms, + means audio late)", itoa(int64(pair.Sync.OffsetMS)))
			writeRow(&b, "    Confidence", ftoa(pair.Sync.Confidence))
			writeRow(&b, "    Drift (ppm)", itoa(int64(pair.Sync.DriftPPM)))
		} else if pair.Sync.ErrorCode != "" {
			writeRow(&b, "    Error code", pair.Sync.ErrorCode)
			writeRow(&b, "    Error message", pair.Sync.ErrorMessage)
		}
		writeFindings(&b, pair.Findings, "    ")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func writeRow(b *strings.Builder, label, value string) {
	b.WriteString("  ")
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

func writeFindings(b *strings.Builder, findings []Finding, indent string) {
	for _, finding := range findings {
		b.WriteString(indent)
		b.WriteString("- [")
		b.WriteString(strings.ToUpper(finding.Severity))
		b.WriteString("] ")
		b.WriteString(finding.Code)
		b.WriteString(": ")
		b.WriteString(finding.Message)
		b.WriteString("\n")
	}
}

func boolText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

