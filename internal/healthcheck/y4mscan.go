package healthcheck

import (
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

// errHeaderTooLong aborts a header-line read once the bounded buffer is full.
var errHeaderTooLong = errors.New("y4m header line exceeds the bounded buffer")

// y4mHeader holds the validated geometry extracted from the Y4M header line.
type y4mHeader struct {
	width     int
	height    int
	fpsNum    int
	fpsDen    int
	chroma    string
	interlace string
}

// flashState retains one mean-luma value per frame (capped at the service's
// MaxVideoFrames) instead of frame payloads, so memory stays bounded even for
// long streams.
type flashState struct {
	luma []float64
}

// ScanY4M streams a Y4M file, validating every header token and every frame
// boundary against the same rules as media.InspectVideo and
// parseY4MHeader, while applying domain.MaxVideoBytes and MaxVideoFrames.
func ScanY4M(path string, budget int64) (*VideoReport, error) {
	return scanY4MWithHook(path, budget, nil)
}

func scanY4MWithHook(path string, budget int64, hook func(string)) (*VideoReport, error) {
	report := &VideoReport{Admittable: true, Findings: []Finding{}}
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

	if info.Size() > int64(domain.MaxVideoBytes) {
		rejectVideoOnly(report, "payload_too_large",
			"video is %d bytes and exceeds the %d byte admission limit",
			info.Size(), domain.MaxVideoBytes)
	}

	// Read the single Y4M header line. Y4M headers are short; a line beyond a
	// sane bound is rejected rather than allowed to grow memory without limit.
	var line strings.Builder
	const maxHeaderLine = 4096
	if err := reader.readUntil('\n', func(b byte) error {
		if line.Len() >= maxHeaderLine {
			return errHeaderTooLong
		}
		line.WriteByte(b)
		return nil
	}); err != nil {
		if err == errHeaderTooLong {
			return rejectVideo(report, "invalid_video", "Y4M header line exceeds the %d byte bound", maxHeaderLine)
		}
		if err == io.ErrUnexpectedEOF {
			return rejectVideo(report, "invalid_video", "Y4M header line is incomplete")
		}
		return rejectVideo(report, "read_failed", "media read failed: %v", err)
	}
	header, err := parseY4MHeader(line.String())
	if err != nil {
		scanErr := asScanError(err)
		message := err.Error()
		if scanErr != nil {
			message = scanErr.message
		}
		return rejectVideo(report, "invalid_video", "%s", message)
	}

	frameBytes := header.width*header.height + 2*(header.width/2)*(header.height/2)
	if frameBytes <= 0 {
		return rejectVideo(report, "invalid_video", "Y4M frame dimensions are invalid")
	}
	planeSize := header.width * header.height

	flash := &flashState{luma: make([]float64, 0, 64)}
	frameCount := 0
	frameHeaderBuf := make([]byte, 0, 64)
	chunk := make([]byte, 32*1024)

	for reader.position() < info.Size() {
		// Frame marker line: "FRAME" plus optional per-frame parameters.
		frameHeaderBuf = frameHeaderBuf[:0]
		if err := reader.readUntil('\n', func(b byte) error {
			if len(frameHeaderBuf) < 4096 {
				frameHeaderBuf = append(frameHeaderBuf, b)
			}
			return nil
		}); err != nil {
			if err == io.ErrUnexpectedEOF {
				return rejectVideo(report, "invalid_video", "Y4M frame header is incomplete")
			}
			return rejectVideo(report, "read_failed", "media read failed: %v", err)
		}
		fields := strings.Fields(string(frameHeaderBuf))
		if len(fields) == 0 || fields[0] != "FRAME" {
			return rejectVideo(report, "invalid_video", "expected a Y4M FRAME marker")
		}

		// Stream the frame payload. Only the luma plane needs accumulation for
		// flash detection; chroma bytes pass through the digest and are dropped.
		var lumaSum int64
		frameConsumed := 0
		remaining := frameBytes
		for remaining > 0 {
			take := len(chunk)
			if take > remaining {
				take = remaining
			}
			buf := chunk[:take]
			if err := reader.readExact(buf); err != nil {
				if err == io.ErrUnexpectedEOF {
					return rejectVideo(report, "invalid_video", "Y4M frame payload is truncated")
				}
				return rejectVideo(report, "read_failed", "media read failed: %v", err)
			}
			if frameConsumed < planeSize {
				usable := take
				if frameConsumed+usable > planeSize {
					usable = planeSize - frameConsumed
				}
				for _, value := range buf[:usable] {
					lumaSum += int64(value)
				}
			}
			frameConsumed += take
			remaining -= take
		}

		frameCount++
		if frameCount > domain.MaxVideoFrames {
			return rejectVideo(report, "invalid_video", "Y4M frame count exceeds the limit of %d", domain.MaxVideoFrames)
		}
		flash.luma = append(flash.luma, float64(lumaSum)/float64(planeSize))
	}

	if frameCount == 0 {
		return rejectVideo(report, "invalid_video", "Y4M payload has no frames")
	}

	report.Width = header.width
	report.Height = header.height
	report.FPSNum = header.fpsNum
	report.FPSDen = header.fpsDen
	report.FrameCount = frameCount
	report.DurationMS = frameCount * 1000 * header.fpsDen / header.fpsNum
	report.Chroma = header.chroma

	if report.Admittable {
		eventMS, prominence, found, reason := resolveFlash(flash.luma, header.fpsNum, header.fpsDen)
		report.FlashFound = found
		report.Prominence = prominence
		if found {
			report.FlashEventMS = eventMS
		} else {
			report.Findings = append(report.Findings, Finding{
				Code:     "flash_detection_failed",
				Severity: "info",
				Message:  reason,
			})
		}
	}

	return report, nil
}

// resolveFlash reproduces media.detectPictureFlash: median luma baseline,
// max delta contrast gate, 0.55 delta threshold, first crossing event.
func resolveFlash(luma []float64, fpsNum, fpsDen int) (int, float64, bool, string) {
	baselineValues := append([]float64(nil), luma...)
	sort.Float64s(baselineValues)
	baseline := baselineValues[len(baselineValues)/2]
	maxValue := 0.0
	for _, value := range luma {
		if value > maxValue {
			maxValue = value
		}
	}
	delta := maxValue - baseline
	if delta < 18 {
		return 0, 0, false, "picture flash has insufficient contrast"
	}
	threshold := baseline + delta*0.55
	eventIndex := -1
	for index, value := range luma {
		if value >= threshold {
			eventIndex = index
			break
		}
	}
	if eventIndex < 0 {
		return 0, 0, false, "picture flash was not found"
	}
	eventMS := eventIndex * 1000 * fpsDen / fpsNum
	prominence := math.Min(1, delta/80)
	return eventMS, math.Round(prominence*1000) / 1000, true, ""
}

// parseY4MHeader is a line-for-line port of media.parseY4MHeader so rejection
// reasons stay identical to the service.
func parseY4MHeader(line string) (y4mHeader, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "YUV4MPEG2" {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "payload is not Y4M"}
	}
	header := y4mHeader{fpsDen: 1, chroma: "C420jpeg", interlace: "p"}
	for _, token := range fields[1:] {
		if len(token) < 2 {
			return y4mHeader{}, &scanError{code: "invalid_video", message: "invalid Y4M header token"}
		}
		value := token[1:]
		switch token[0] {
		case 'W':
			number, err := strconv.Atoi(value)
			if err != nil {
				return y4mHeader{}, &scanError{code: "invalid_video", message: "invalid Y4M width"}
			}
			header.width = number
		case 'H':
			number, err := strconv.Atoi(value)
			if err != nil {
				return y4mHeader{}, &scanError{code: "invalid_video", message: "invalid Y4M height"}
			}
			header.height = number
		case 'F':
			parts := strings.Split(value, ":")
			if len(parts) != 2 {
				return y4mHeader{}, &scanError{code: "invalid_video", message: "invalid Y4M frame rate"}
			}
			num, errNum := strconv.Atoi(parts[0])
			den, errDen := strconv.Atoi(parts[1])
			if errNum != nil || errDen != nil || num <= 0 || den <= 0 {
				return y4mHeader{}, &scanError{code: "invalid_video", message: "invalid Y4M frame rate"}
			}
			header.fpsNum = num
			header.fpsDen = den
		case 'C':
			header.chroma = token
		case 'A':
			// aspect is accepted but does not affect admission.
		case 'I':
			header.interlace = value
		}
	}
	if header.width <= 0 || header.height <= 0 {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "Y4M width and height are required"}
	}
	if header.width%2 != 0 || header.height%2 != 0 {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "C420 dimensions must be even"}
	}
	if header.width > domain.MaxVideoWidth || header.height > domain.MaxVideoHeight {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "Y4M dimensions exceed the limit"}
	}
	if header.fpsNum <= 0 || header.fpsNum > 120 || header.fpsDen > 1001 {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "Y4M frame rate is outside the accepted range"}
	}
	if header.chroma != "C420jpeg" && header.chroma != "C420mpeg2" && header.chroma != "C420" {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "only planar C420 Y4M is accepted"}
	}
	if header.interlace != "p" {
		return y4mHeader{}, &scanError{code: "invalid_video", message: "only progressive Y4M is accepted"}
	}
	return header, nil
}

func rejectVideo(report *VideoReport, code, format string, args ...any) (*VideoReport, error) {
	rejectVideoOnly(report, code, format, args...)
	return report, &scanError{code: code, message: report.Findings[len(report.Findings)-1].Message}
}

func rejectVideoOnly(report *VideoReport, code, format string, args ...any) {
	report.Admittable = false
	report.Findings = append(report.Findings, Finding{
		Code:     code,
		Severity: "reject",
		Message:  sprintf(format, args...),
	})
}
