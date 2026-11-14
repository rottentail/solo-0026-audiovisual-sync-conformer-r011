package healthcheck

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
)

// writeFixture writes bytes into a fresh, test-owned directory. Test fixtures
// never live next to package source, so no planted module can shadow imports.
func writeFixture(t *testing.T, name string, payload []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestScanWAVMatchesServiceInspection(t *testing.T) {
	audio, _, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration audio: %v", err)
	}
	path := writeFixture(t, "calib.wav", audio)

	report, err := ScanWAV(path, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanWAV returned error: %v", err)
	}
	clip, err := media.InspectAudio(audio)
	if err != nil {
		t.Fatalf("service InspectAudio: %v", err)
	}
	info := clip.Info

	if !report.Admittable {
		t.Fatalf("calibration WAV must be admittable, findings=%v", report.Findings)
	}
	if report.SampleRate != info.SampleRate {
		t.Errorf("sample rate: got %d want %d", report.SampleRate, info.SampleRate)
	}
	if report.Channels != info.Channels {
		t.Errorf("channels: got %d want %d", report.Channels, info.Channels)
	}
	if report.FrameCount != info.FrameCount {
		t.Errorf("frame count: got %d want %d", report.FrameCount, info.FrameCount)
	}
	if report.DurationMS != info.DurationMS {
		t.Errorf("duration: got %d want %d", report.DurationMS, info.DurationMS)
	}
	if report.PeakDBFS != info.PeakDBFS {
		t.Errorf("peak: got %.2f want %.2f", report.PeakDBFS, info.PeakDBFS)
	}
	if report.RMSDBFS != info.RMSDBFS {
		t.Errorf("rms: got %.2f want %.2f", report.RMSDBFS, info.RMSDBFS)
	}
	if report.Snapshot.DigestSHA256 != info.Digest {
		t.Errorf("digest mismatch: got %s want %s", report.Snapshot.DigestSHA256, info.Digest)
	}
}

func TestScanY4MMatchesServiceInspection(t *testing.T) {
	_, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration video: %v", err)
	}
	path := writeFixture(t, "calib.y4m", video)

	report, err := ScanY4M(path, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanY4M returned error: %v", err)
	}
	clip, err := media.InspectVideo(video)
	if err != nil {
		t.Fatalf("service InspectVideo: %v", err)
	}
	info := clip.Info

	if !report.Admittable {
		t.Fatalf("calibration Y4M must be admittable, findings=%v", report.Findings)
	}
	if report.Width != info.Width || report.Height != info.Height {
		t.Errorf("geometry: got %dx%d want %dx%d", report.Width, report.Height, info.Width, info.Height)
	}
	if report.FPSNum != info.FPSNum || report.FPSDen != info.FPSDen {
		t.Errorf("fps: got %d:%d want %d:%d", report.FPSNum, report.FPSDen, info.FPSNum, info.FPSDen)
	}
	if report.FrameCount != info.FrameCount {
		t.Errorf("frame count: got %d want %d", report.FrameCount, info.FrameCount)
	}
	if report.DurationMS != info.DurationMS {
		t.Errorf("duration: got %d want %d", report.DurationMS, info.DurationMS)
	}
	if report.Chroma != info.Chroma {
		t.Errorf("chroma: got %s want %s", report.Chroma, info.Chroma)
	}
	if report.Snapshot.DigestSHA256 != info.Digest {
		t.Errorf("digest mismatch: got %s want %s", report.Snapshot.DigestSHA256, info.Digest)
	}
}

// TestStreamingDetectorsMatchServiceSignal runs the full calibration pair
// through both code paths and requires byte-identical measurement fields.
func TestStreamingDetectorsMatchServiceSignal(t *testing.T) {
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration pair: %v", err)
	}
	measurement, err := media.AnalyzeSync(audio, video)
	if err != nil {
		t.Fatalf("service AnalyzeSync: %v", err)
	}

	audioPath := writeFixture(t, "calib.wav", audio)
	videoPath := writeFixture(t, "calib-video.y4m", video)
	audioReport, err := ScanWAV(audioPath, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanWAV: %v", err)
	}
	videoReport, err := ScanY4M(videoPath, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanY4M: %v", err)
	}

	if !audioReport.OnsetFound {
		t.Fatalf("streaming onset not found: %v", audioReport.Findings)
	}
	if !videoReport.FlashFound {
		t.Fatalf("streaming flash not found: %v", videoReport.Findings)
	}
	if audioReport.OnsetEventMS != measurement.AudioEventMS {
		t.Errorf("audio event: got %d want %d", audioReport.OnsetEventMS, measurement.AudioEventMS)
	}
	if videoReport.FlashEventMS != measurement.VideoEventMS {
		t.Errorf("video event: got %d want %d", videoReport.FlashEventMS, measurement.VideoEventMS)
	}
	if audioReport.Prominence != measurement.AudioProminence {
		t.Errorf("audio prominence: got %.3f want %.3f", audioReport.Prominence, measurement.AudioProminence)
	}
	if videoReport.Prominence != measurement.VideoProminence {
		t.Errorf("video prominence: got %.3f want %.3f", videoReport.Prominence, measurement.VideoProminence)
	}

	sync := evaluateSync(audioReport, videoReport, Policy{
		MinConfidence: domain.DefaultMinConf,
		MaxOffsetMS:   domain.DefaultMaxOffsetMS,
	})
	if sync.Status != SyncReady {
		t.Fatalf("sync status: got %s (%s)", sync.Status, sync.ErrorMessage)
	}
	if sync.OffsetMS != measurement.OffsetMS {
		t.Errorf("offset: got %d want %d", sync.OffsetMS, measurement.OffsetMS)
	}
	if sync.Confidence != measurement.Confidence {
		t.Errorf("confidence: got %.3f want %.3f", sync.Confidence, measurement.Confidence)
	}
	if sync.DriftPPM != measurement.DriftPPM {
		t.Errorf("drift: got %d want %d", sync.DriftPPM, measurement.DriftPPM)
	}
}

func TestRejectsInvalidWAV(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"not-riff":         []byte("XXXXXXXXXXXX"),
		"truncated-fmt":    truncatedFmtWAV(),
		"stereo-24bit":     buildWAV(2, 48000, 24, []int16{1, 2, 3, 4}),
		"unsupported-rate": handBuildWAV(1, 4000, 16, make([]int16, 4800)),
		"three-channels":   handBuildWAV(3, 48000, 16, make([]int16, 4800)),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFixture(t, "bad.wav", payload)
			report, _ := ScanWAV(path, minBudgetBytes)
			if report == nil {
				t.Fatalf("expected a report even on rejection")
			}
			if report.Admittable {
				t.Fatalf("%s must not be admittable", name)
			}
			if report.Snapshot.DigestSHA256 == "" {
				t.Fatalf("digest must still describe the bytes that were read")
			}
		})
	}
}

func TestRejectsInvalidY4M(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"not-y4m":        []byte("NOTY4M\n"),
		"missing-dims":   []byte("YUV4MPEG2 F24:1 Ip C420jpeg\n"),
		"odd-dims":       []byte("YUV4MPEG2 W95 H54 F24:1 Ip C420jpeg\n"),
		"wrong-chroma":   []byte("YUV4MPEG2 W96 H54 F24:1 Ip C422\n"),
		"interlaced":     []byte("YUV4MPEG2 W96 H54 F24:1 It C420jpeg\n"),
		"truncated-frame": append([]byte("YUV4MPEG2 W96 H54 F24:1 Ip C420jpeg\nFRAME\n"), make([]byte, 100)...),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFixture(t, "bad.y4m", payload)
			report, _ := ScanY4M(path, minBudgetBytes)
			if report == nil {
				t.Fatalf("expected a report even on rejection")
			}
			if report.Admittable {
				t.Fatalf("%s must not be admittable", name)
			}
		})
	}
}

func TestOversizedFilesAreRejected(t *testing.T) {
	// A valid WAV above the 8 MiB admission limit.
	sampleCount := (domain.MaxAudioBytes/2 + 1024)
	samples := make([]int16, sampleCount)
	for i := range samples {
		samples[i] = int16(i%200 - 100)
	}
	payload, err := media.EncodePCM16(48000, 1, samples)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := writeFixture(t, "big.wav", payload)
	report, _ := ScanWAV(path, minBudgetBytes)
	if report.Admittable {
		t.Fatalf("oversized WAV must be rejected")
	}
	if !hasFinding(report.Findings, "payload_too_large") {
		t.Fatalf("expected payload_too_large finding, got %v", report.Findings)
	}
}

func TestLargeStreamStaysWithinBudget(t *testing.T) {
	// Ten seconds of 48 kHz stereo audio (~1.9 MB) scanned with the smallest
	// possible block, proving memory does not scale with file length.
	samples := make([]int16, 48000*2*10)
	eventSample := 48000 * 2 * 2 // two seconds in, stereo interleaved
	for i := eventSample; i < len(samples); i++ {
		envelope := math.Exp(-float64(i-eventSample) / float64(48000*2) * 7)
		samples[i] = int16(12000 * envelope)
	}
	payload, err := media.EncodePCM16(48000, 2, samples)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := writeFixture(t, "long.wav", payload)

	before := allocBytes()
	report, err := ScanWAV(path, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanWAV: %v", err)
	}
	after := allocBytes()
	if !report.Admittable || !report.OnsetFound {
		t.Fatalf("long stream should admitt with an onset, findings=%v", report.Findings)
	}
	// The whole payload is far larger than the budget; a naive load would
	// dwarf this ceiling.
	if int64(after-before) > int64(minBudgetBytes)*4 {
		t.Fatalf("allocation grew by %d bytes for a %d byte file under a %d byte budget",
			after-before, len(payload), minBudgetBytes)
	}
}

func TestRunFlagsFileChangedDuringScan(t *testing.T) {
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "take.wav")
	videoPath := filepath.Join(dir, "take.y4m")
	if err := os.WriteFile(audioPath, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, video, 0o600); err != nil {
		t.Fatal(err)
	}

	// Baseline: untouched bytes must be admittable.
	stable, err := Run(Options{Root: dir, MemoryBudgetBytes: minBudgetBytes})
	if err != nil {
		t.Fatalf("stable run: %v", err)
	}
	if len(stable.Pairs) != 1 || stable.Pairs[0].Verdict != VerdictAccept {
		t.Fatalf("stable pair must be admittable: %+v", stable.Pairs)
	}

	// Concurrent case: the scan hook rewrites the WAV after the bracket's
	// initial stat but while the file is being streamed. The bracket must
	// invalidate the pair rather than emit a confident sync conclusion.
	longSamples := make([]int16, 48000*8)
	for i := 48000; i < len(longSamples); i++ {
		longSamples[i] = int16(8000 * math.Exp(-float64(i-48000)/48000*7))
	}
	longWAV, err := media.EncodePCM16(48000, 1, longSamples)
	if err != nil {
		t.Fatalf("encode long wav: %v", err)
	}
	if err := os.WriteFile(audioPath, longWAV, 0o600); err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte{}, longWAV...)
	mutated[len(mutated)-100] ^= 0xFF

	changed, err := Run(Options{
		Root:              dir,
		MemoryBudgetBytes: minBudgetBytes,
		VerifyContent:     true,
		scanHook: func(path string) {
			if filepath.Base(path) == "take.wav" {
				_ = os.WriteFile(path, mutated, 0o600)
			}
		},
	})
	if err != nil {
		t.Fatalf("changed run: %v", err)
	}
	pair := changed.Pairs[0]
	if pair.Verdict != VerdictIndeterminate || !pair.ChangedDuringScan {
		t.Fatalf("concurrently mutated pair must be indeterminate and flagged changed: %+v", pair)
	}
	if pair.Sync.Status == SyncReady {
		t.Fatalf("a changed pair must never expose a ready sync result")
	}
}

func TestVerifyContentDetectsSameSizeRewrite(t *testing.T) {
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	dir := t.TempDir()
	audioPath := filepath.Join(dir, "take.wav")
	videoPath := filepath.Join(dir, "take.y4m")
	if err := os.WriteFile(audioPath, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, video, 0o600); err != nil {
		t.Fatal(err)
	}
	// Directly exercise the bracket helper with a same-size, same-mtime style
	// rewrite: content verification must catch the digest delta.
	before, err := os.Stat(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ScanWAV(audioPath, minBudgetBytes)
	if err != nil {
		t.Fatalf("ScanWAV: %v", err)
	}
	mutated := append([]byte{}, audio...)
	mutated[len(mutated)-50] ^= 0x01
	if err := os.WriteFile(audioPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	// Preserve the original mtime so only the digest can reveal the rewrite.
	if err := os.Chtimes(audioPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	pair := &PairReport{Findings: []Finding{}}
	changed := verifyUnchanged(audioPath, before, report.Snapshot.DigestSHA256,
		Options{VerifyContent: true, MemoryBudgetBytes: minBudgetBytes}, pair, "take.wav")
	if !changed {
		t.Fatalf("content verification must detect a same-size rewrite")
	}
}

func hasFinding(findings []Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func truncatedFmtWAV() []byte {
	base := buildWAV(1, 48000, 16, []int16{1, 2, 3, 4})
	// Shrink the declared fmt chunk size below 16 and corrupt the chunk walk.
	binary.LittleEndian.PutUint32(base[16:20], 8)
	return base[:24]
}

func buildWAV(channels, sampleRate, bits int, samples []int16) []byte {
	if bits != 16 {
		return handBuildWAV(channels, sampleRate, bits, samples)
	}
	payload, err := media.EncodePCM16(sampleRate, channels, samples)
	if err != nil {
		panic(err)
	}
	return payload
}

// handBuildWAV constructs a raw RIFF container without the service encoder's
// admission checks, so tests can exercise metadata the encoder refuses.
func handBuildWAV(channels, sampleRate, bits int, samples []int16) []byte {
	bytesPerSample := bits / 8
	var b []byte
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(sampleRate))
	b = binary.LittleEndian.AppendUint32(b, uint32(sampleRate*channels*bytesPerSample))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels*bytesPerSample))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	b = append(b, "data"...)
	dataBytes := len(samples) * bytesPerSample
	b = binary.LittleEndian.AppendUint32(b, uint32(dataBytes))
	if bits == 16 {
		for _, sample := range samples {
			b = binary.LittleEndian.AppendUint16(b, uint16(sample))
		}
	} else {
		for range samples {
			for i := 0; i < bytesPerSample; i++ {
				b = append(b, 0)
			}
		}
	}
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	return b
}

func allocBytes() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.TotalAlloc
}
