package healthcheck

import (
	"encoding/binary"
	"io"
	"math"
	"os"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// waveFormat mirrors the fields the service extracts from a WAV fmt chunk.
type waveFormat struct {
	audioFormat   uint16
	channels      uint16
	sampleRate    uint32
	blockAlign    uint16
	bitsPerSample uint16
}

// onsetState holds the constant-memory state of the streaming onset detector.
// It reproduces media.detectAudioOnset exactly while retaining only one RMS
// value per 5 ms window plus counters: no sample array is ever held.
type onsetState struct {
	sampleRate       int
	channels         int
	windowSamples    int
	windowCollected  int
	windowSumSquares float64
	windowRMS        []float64
	peak             float64
	noiseWindows     []float64 // at most the first 12 window RMS values
	eventWindow      int       // -1 until an onset window is chosen
	preciseCounter   int       // samples counted since the event window began
	onsetSample      int       // absolute sample index of the onset, -1 if absent
}

func newOnsetState(sampleRate, channels int) *onsetState {
	windowFrames := sampleRate / 200
	if windowFrames < 1 {
		windowFrames = 1
	}
	return &onsetState{
		sampleRate:    sampleRate,
		channels:      channels,
		windowSamples: windowFrames * channels,
		windowRMS:     make([]float64, 0, 64),
		noiseWindows:  make([]float64, 0, 12),
		eventWindow:   -1,
		onsetSample:   -1,
	}
}

// pushWindow closes one RMS window. Because the global peak is known at every
// prefix, the same threshold/neighbor rule used by the service can select the
// event window as soon as it closes.
func (o *onsetState) pushWindow(rms float64) {
	index := len(o.windowRMS)
	o.windowRMS = append(o.windowRMS, rms)
	if rms > o.peak {
		o.peak = rms
	}
	if len(o.noiseWindows) < 12 {
		o.noiseWindows = append(o.noiseWindows, rms)
	}
	if o.eventWindow >= 0 || index < 4 {
		return
	}
	threshold := o.peak * 0.18
	if rms < threshold {
		return
	}
	quietNeighbors := 0
	for lookback := 1; lookback <= 4 && index-lookback >= 0; lookback++ {
		if o.windowRMS[index-lookback] < threshold*0.35 {
			quietNeighbors++
		}
	}
	if quietNeighbors >= 3 {
		o.eventWindow = index
		o.preciseCounter = 0
	}
}

// pushSample feeds one interleaved PCM sample (a single int16 channel value).
func (o *onsetState) pushSample(sample int16) {
	value := float64(sample)

	// Once the event window is selected, precise localization scans from its
	// first sample for |sample| >= peak*0.12, matching the service loop.
	if o.eventWindow >= 0 && o.onsetSample < 0 {
		if math.Abs(value) >= o.peak*0.12 {
			o.onsetSample = o.eventWindow*o.windowSamples + o.preciseCounter
		}
		o.preciseCounter++
	}

	o.windowSumSquares += value * value
	o.windowCollected++
	if o.windowCollected == o.windowSamples {
		rms := math.Sqrt(o.windowSumSquares / float64(o.windowSamples))
		o.pushWindow(rms)
		o.windowCollected = 0
		o.windowSumSquares = 0
	}
}

// resolve applies the service's final gates and returns the same shaped
// result, including identical failure reasons.
func (o *onsetState) resolve(totalSamples int) (eventMS int, prominence float64, found bool, reason string) {
	if len(o.windowRMS) < 4 {
		return 0, 0, false, "audio track is too short for onset detection"
	}
	if o.peak < 40 {
		return 0, 0, false, "audio onset has insufficient amplitude"
	}
	if o.eventWindow < 0 {
		return 0, 0, false, "audio onset was not found"
	}
	startSample := o.eventWindow * o.windowSamples
	if o.onsetSample < startSample {
		// The service's precise loop reaches end of stream without a hit and
		// reports the final scanned position.
		o.onsetSample = totalSamples
	}
	if o.onsetSample > totalSamples {
		o.onsetSample = totalSamples
	}
	eventMS = o.onsetSample * 1000 / (o.sampleRate * o.channels)

	noiseFloor := 1.0
	if len(o.noiseWindows) > 0 {
		var sum float64
		for _, value := range o.noiseWindows {
			sum += value
		}
		noiseFloor = math.Max(1, sum/float64(len(o.noiseWindows)))
	}
	prominence = math.Min(1, math.Log10(1+o.peak/noiseFloor)/2)
	return eventMS, math.Round(prominence*1000) / 1000, true, ""
}

// ScanWAV streams a WAV file and returns a full audio report. It never
// allocates more than fixed-size buffers regardless of file length, and it
// applies every admission rule enforced by media.InspectAudio plus the service
// size budget in domain.MaxAudioBytes.
func ScanWAV(path string, budget int64) (*AudioReport, error) {
	return scanWAVWithHook(path, budget, nil)
}

func scanWAVWithHook(path string, budget int64, hook func(string)) (*AudioReport, error) {
	report := &AudioReport{Admittable: true, Findings: []Finding{}}
	reader, info, err := openBounded(path, int(budget))
	if err != nil {
		report.Admittable = false
		report.Findings = append(report.Findings, Finding{
			Code: "read_failed", Severity: "reject", Message: err.Error(),
		})
		return report, err
	}
	report.Snapshot = snapshotInfo(path, info, "")

	if hook != nil {
		hook(path)
	}

	defer reader.close()
	// Finalize the digest on every exit so the report always describes the
	// exact bytes streamed, even when a rule rejects the file early.
	defer func() {
		report.Snapshot.DigestSHA256 = reader.digest()
	}()

	header := make([]byte, 12)
	if err := reader.readExact(header); err != nil {
		return rejectAudio(report, "invalid_audio", "audio payload is shorter than a RIFF header")
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return rejectAudio(report, "invalid_audio", "audio payload is not RIFF/WAVE")
	}
	riffSize := int64(binary.LittleEndian.Uint32(header[4:8]))
	if riffSize < 4 || riffSize+8 > info.Size() {
		return rejectAudio(report, "invalid_audio", "RIFF size does not fit the payload")
	}
	if info.Size() > int64(domain.MaxAudioBytes) {
		rejectAudioOnly(report, "payload_too_large",
			"audio is %d bytes and exceeds the %d byte admission limit",
			info.Size(), domain.MaxAudioBytes)
	}

	var format waveFormat
	haveFormat := false
	dataStarted := false
	var onset *onsetState

	var peak float64
	var squareSum float64
	var sampleCount int64
	var dataByteCount int64
	sampleBuf := make([]byte, 4096)

	// Chunk walk mirrors media.InspectAudio, including odd-byte padding. The
	// service validates riffSize against the payload length but then walks the
	// full payload, so trailing bytes are still parsed as chunks.
	limit := info.Size()
	offset := int64(12)
	for offset < limit {
		if limit-offset < 8 {
			return rejectAudio(report, "invalid_audio", "truncated WAV chunk header")
		}
		chunkHeader := make([]byte, 8)
		if err := reader.readExact(chunkHeader); err != nil {
			if err == io.ErrUnexpectedEOF {
				return rejectAudio(report, "invalid_audio", "truncated WAV chunk header")
			}
			return rejectAudio(report, "read_failed", "media read failed: %v", err)
		}
		offset += 8
		chunkID := string(chunkHeader[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		if chunkSize < 0 || offset+chunkSize > limit {
			return rejectAudio(report, "invalid_audio", "WAV chunk %q exceeds payload", chunkID)
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return rejectAudio(report, "invalid_audio", "WAV format chunk is too short")
			}
			body := make([]byte, 16)
			if err := reader.readExact(body); err != nil {
				return rejectAudio(report, "read_failed", "media read failed: %v", err)
			}
			if err := reader.discard(chunkSize - 16); err != nil {
				return rejectAudio(report, "read_failed", "media read failed: %v", err)
			}
			offset += chunkSize
			format = waveFormat{
				audioFormat:   binary.LittleEndian.Uint16(body[0:2]),
				channels:      binary.LittleEndian.Uint16(body[2:4]),
				sampleRate:    binary.LittleEndian.Uint32(body[4:8]),
				blockAlign:    binary.LittleEndian.Uint16(body[12:14]),
				bitsPerSample: binary.LittleEndian.Uint16(body[14:16]),
			}
			haveFormat = true
		case "data":
			// The service captures data chunks regardless of their position
			// relative to fmt and validates the format fields after the walk,
			// so no ordering rejection is applied here.
			if !dataStarted {
				onset = newOnsetState(maxInt(int(format.sampleRate), 1), maxInt(int(format.channels), 1))
				dataStarted = true
			}
			dataByteCount += chunkSize
			remaining := chunkSize
			for remaining > 0 {
				take := int64(len(sampleBuf))
				if take > remaining {
					take = remaining
				}
				buf := sampleBuf[:take]
				if err := reader.readExact(buf); err != nil {
					return rejectAudio(report, "invalid_audio", "WAV data chunk is truncated")
				}
				for i := 0; i+1 < len(buf); i += 2 {
					sample := int16(binary.LittleEndian.Uint16(buf[i : i+2]))
					sampleCount++
					abs := float64(sample)
					if abs < 0 {
						abs = -abs
					}
					if abs > peak {
						peak = abs
					}
					squareSum += abs * abs
					onset.pushSample(sample)
				}
				remaining -= take
			}
			offset += chunkSize
		default:
			if err := reader.discard(chunkSize); err != nil {
				return rejectAudio(report, "read_failed", "media read failed: %v", err)
			}
			offset += chunkSize
		}
		if chunkSize%2 == 1 {
			if err := reader.discard(1); err != nil {
				return rejectAudio(report, "read_failed", "media read failed: %v", err)
			}
			offset++
		}
	}

	if !haveFormat {
		return rejectAudio(report, "invalid_audio", "WAV format chunk is missing")
	}
	if !dataStarted {
		return rejectAudio(report, "invalid_audio", "WAV data chunk is missing")
	}
	if format.audioFormat != 1 {
		rejectAudioOnly(report, "invalid_audio", "only integer PCM audio is accepted")
	}
	if format.channels != 1 && format.channels != 2 {
		rejectAudioOnly(report, "invalid_audio", "audio must have one or two channels")
	}
	if format.bitsPerSample != 16 {
		rejectAudioOnly(report, "invalid_audio", "audio must use 16-bit samples")
	}
	if format.sampleRate < 8000 || format.sampleRate > 192000 {
		rejectAudioOnly(report, "invalid_audio", "sample rate is outside the accepted range")
	}
	blockAlign := int(format.channels) * 2
	if int(format.blockAlign) != blockAlign {
		rejectAudioOnly(report, "invalid_audio", "WAV block alignment is inconsistent")
	}
	if dataByteCount == 0 || dataByteCount%int64(blockAlign) != 0 {
		rejectAudioOnly(report, "invalid_audio", "WAV sample bytes are incomplete")
	}

	report.SampleRate = int(format.sampleRate)
	report.Channels = int(format.channels)
	report.BitDepth = 16
	// The service frames are int16 values divided by channel count. An odd
	// final byte is already rejected above, so sampleCount equals data bytes/2.
	// Guard against invalid-but-present format values to avoid divide-by-zero.
	safeChannels := int(format.channels)
	if safeChannels < 1 {
		safeChannels = 1
	}
	safeRate := int(format.sampleRate)
	if safeRate < 1 {
		safeRate = 1
	}
	report.FrameCount = int(sampleCount) / safeChannels
	report.DurationMS = report.FrameCount * 1000 / safeRate
	rms := 0.0
	if sampleCount > 0 {
		rms = math.Sqrt(squareSum / float64(sampleCount))
	}
	report.PeakDBFS = amplitudeDBFS(peak)
	report.RMSDBFS = amplitudeDBFS(rms)

	if report.Admittable {
		eventMS, prom, found, reason := onset.resolve(int(sampleCount))
		report.OnsetFound = found
		report.Prominence = prom
		if found {
			report.OnsetEventMS = eventMS
		} else {
			report.Findings = append(report.Findings, Finding{
				Code:     "onset_detection_failed",
				Severity: "info",
				Message:  reason,
			})
		}
	}

	return report, nil
}

// rejectAudio marks the report and returns a deterministic scan error so the
// driver stops reading this file.
func rejectAudio(report *AudioReport, code, format string, args ...any) (*AudioReport, error) {
	rejectAudioOnly(report, code, format, args...)
	return report, &scanError{code: code, message: report.Findings[len(report.Findings)-1].Message}
}

func rejectAudioOnly(report *AudioReport, code, format string, args ...any) {
	report.Admittable = false
	report.Findings = append(report.Findings, Finding{
		Code:     code,
		Severity: "reject",
		Message:  sprintf(format, args...),
	})
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 {
		return -120
	}
	db := 20 * math.Log10(value/32768)
	if db < -120 {
		return -120
	}
	return math.Round(db*100) / 100
}

func snapshotInfo(path string, info os.FileInfo, digest string) FileSnapshot {
	return FileSnapshot{
		Path:         path,
		ModTime:      info.ModTime().UTC(),
		Size:         info.Size(),
		Mode:         uint32(info.Mode().Perm()),
		DigestSHA256: digest,
	}
}
