package healthcheck

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
)

func TestOddDataBytesRejectedLikeService(t *testing.T) {
	valid := handBuildWAV(1, 48000, 16, make([]int16, 4800))
	// Drop one byte from the data span and patch both RIFF and data sizes.
	trimmed := append([]byte{}, valid[:len(valid)-1]...)
	// data chunk size field sits at offset 40.
	putUint32(trimmed, 40, uint32(4800*2-1))
	putUint32(trimmed, 4, uint32(len(trimmed)-8))

	streamReport, _ := ScanWAV(writeFixture(t, "odd.wav", trimmed), minBudgetBytes)
	if streamReport.Admittable {
		t.Fatalf("odd data bytes must be rejected by the streaming scanner")
	}
	if _, err := media.InspectAudio(trimmed); err == nil {
		t.Fatalf("service must also reject odd data bytes")
	}
}

func TestPairingCollisionsAndUnpaired(t *testing.T) {
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "dup.wav"), audio)
	mustWrite(t, filepath.Join(dir, "DUP.WAV"), audio) // same lowercase pairing key
	mustWrite(t, filepath.Join(dir, "dup.y4m"), video)
	mustWrite(t, filepath.Join(dir, "only.wav"), audio)

	report, err := Run(Options{Root: dir, MemoryBudgetBytes: minBudgetBytes})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byKey := map[string]PairReport{}
	for _, pair := range report.Pairs {
		byKey[pair.Key] = pair
	}
	collision, ok := byKey["dup"]
	if !ok || collision.Verdict != VerdictIndeterminate {
		t.Fatalf("expected indeterminate collision for dup, got %+v", byKey)
	}
	if !hasFinding(collision.Findings, "pairing_collision") {
		t.Fatalf("collision must carry a pairing_collision finding")
	}
	only, ok := byKey["only"]
	if !ok || only.Verdict != VerdictReject || only.Sync.Status != SyncSkippedUnpaired {
		t.Fatalf("expected rejected unpaired group, got %+v", only)
	}
}

func TestPolicyGatesDriveSyncStatus(t *testing.T) {
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "p.wav"), audio)
	mustWrite(t, filepath.Join(dir, "p.y4m"), video)

	// The calibration offset is 125 ms; a 100 ms ceiling must trip the gate.
	report, err := Run(Options{
		Root:              dir,
		MemoryBudgetBytes: minBudgetBytes,
		Policy:            Policy{MinConfidence: domain.DefaultMinConf, MaxOffsetMS: 100},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	pair := report.Pairs[0]
	if pair.Sync.Status != SyncOffsetExceeded {
		t.Fatalf("expected offset_exceeded, got %s (%s)", pair.Sync.Status, pair.Sync.ErrorMessage)
	}

	// An unreachable confidence floor needs a fixture whose prominence is
	// below 1.0: add a constant noise floor so onset prominence drops.
	noisy := make([]int16, 48000)
	for i := range noisy {
		noisy[i] = 400
	}
	eventSample := 48000 / 2
	for i := eventSample; i < len(noisy); i++ {
		envelope := math.Exp(-float64(i-eventSample)/48000*7)
		noisy[i] = int16(400 + 12000*envelope)
	}
	noisyWAV, err := media.EncodePCM16(48000, 1, noisy)
	if err != nil {
		t.Fatalf("encode noisy wav: %v", err)
	}
	noisyDir := t.TempDir()
	mustWrite(t, filepath.Join(noisyDir, "n.wav"), noisyWAV)
	mustWrite(t, filepath.Join(noisyDir, "n.y4m"), video)
	report, err = Run(Options{
		Root:              noisyDir,
		MemoryBudgetBytes: minBudgetBytes,
		Policy:            Policy{MinConfidence: 0.95, MaxOffsetMS: domain.DefaultMaxOffsetMS},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Pairs[0].Sync.Status != SyncLowConfidence {
		t.Fatalf("expected low_confidence, got %s (confidence=%.3f)",
			report.Pairs[0].Sync.Status, report.Pairs[0].Sync.Confidence)
	}
}

func TestFrameLimitRejected(t *testing.T) {
	// Build a Y4M declaring one frame beyond the service cap. Geometry is
	// small (16x16) so the fixture stays tiny.
	width, height := 16, 16
	frameBytes := width*height + 2*(width/2)*(height/2)
	var payload []byte
	payload = append(payload, []byte("YUV4MPEG2 W16 H16 F24:1 Ip A0:0 C420jpeg\n")...)
	for i := 0; i < domain.MaxVideoFrames+1; i++ {
		payload = append(payload, []byte("FRAME\n")...)
		frame := make([]byte, frameBytes)
		payload = append(payload, frame...)
	}
	report, _ := ScanY4M(writeFixture(t, "many.y4m", payload), minBudgetBytes)
	if report.Admittable {
		t.Fatalf("frame count above the cap must be rejected")
	}
	if !hasFinding(report.Findings, "invalid_video") {
		t.Fatalf("expected invalid_video finding, got %v", report.Findings)
	}
}

func mustWrite(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func putUint32(buf []byte, offset int, value uint32) {
	buf[offset] = byte(value)
	buf[offset+1] = byte(value >> 8)
	buf[offset+2] = byte(value >> 16)
	buf[offset+3] = byte(value >> 24)
}

func TestTrailingBytesWalkedLikeService(t *testing.T) {
	valid := handBuildWAV(1, 48000, 16, make([]int16, 4800))
	// Append 4 trailing bytes beyond the RIFF-declared span. The service walks
	// the full payload and rejects the dangling partial chunk header; the
	// streaming scanner must reach the same verdict.
	trailing := append(append([]byte{}, valid...), 0xFF, 0xFF, 0xFF, 0xFF)
	path := writeFixture(t, "trailing.wav", trailing)

	streamReport, _ := ScanWAV(path, minBudgetBytes)
	_, serviceErr := media.InspectAudio(trailing)
	if serviceErr == nil {
		t.Fatalf("service must reject trailing partial chunk")
	}
	if streamReport.Admittable {
		t.Fatalf("streaming scanner must also reject trailing partial chunk")
	}
}
