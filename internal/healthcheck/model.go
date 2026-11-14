package healthcheck

import "time"

// Verdict values mirror the stable decisions an operator can act on.
const (
	// VerdictAccept means both files pass every admission rule the service
	// enforces, so the pair can be submitted as a take.
	VerdictAccept = "admittable"
	// VerdictReject means at least one file fails a rule the service enforces
	// at admission; the service would return a 4xx error.
	VerdictReject = "not_admittable"
	// VerdictIndeterminate means the bytes changed during the scan or could
	// not be fully read, so no trustworthy audio/picture conclusion exists.
	VerdictIndeterminate = "indeterminate"
)

// Sync outcomes describe what the sync-analysis workflow would decide when run
// with the report's policy, without ever calling the service.
const (
	SyncReady                = "ready"
	SyncDetectionFailed      = "sync_detection_failed"
	SyncLowConfidence        = "low_confidence"
	SyncOffsetExceeded       = "offset_exceeded"
	SyncSkippedNotAdmittable = "skipped_not_admittable"
	SyncSkippedChanged       = "skipped_file_changed"
	SyncSkippedUnpaired      = "skipped_unpaired"
)

// Finding is one deterministic probe result. Codes stay stable across runs so
// reports can be diffed mechanically.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // "reject" | "warn" | "info"
	Message  string `json:"message"`
}

// FileSnapshot records the exact identity of the bytes that were read.
type FileSnapshot struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Mode        uint32    `json:"mode"`
	ModTime     time.Time `json:"mod_time"`
	DigestSHA256 string   `json:"sha256"`
}

// AudioReport carries the streaming equivalent of domain.AudioInfo plus the
// admission findings for one WAV file.
type AudioReport struct {
	Snapshot      FileSnapshot `json:"snapshot"`
	Admittable    bool         `json:"admittable"`
	SampleRate    int          `json:"sample_rate,omitempty"`
	Channels      int          `json:"channels,omitempty"`
	BitDepth      int          `json:"bit_depth,omitempty"`
	FrameCount    int          `json:"frame_count,omitempty"`
	DurationMS    int          `json:"duration_ms,omitempty"`
	PeakDBFS      float64      `json:"peak_dbfs,omitempty"`
	RMSDBFS       float64      `json:"rms_dbfs,omitempty"`
	OnsetEventMS  int          `json:"onset_event_ms,omitempty"`
	OnsetFound    bool         `json:"onset_found"`
	Prominence    float64      `json:"prominence,omitempty"`
	Findings      []Finding    `json:"findings"`
}

// VideoReport carries the streaming equivalent of domain.VideoInfo.
type VideoReport struct {
	Snapshot     FileSnapshot `json:"snapshot"`
	Admittable   bool         `json:"admittable"`
	Width        int          `json:"width,omitempty"`
	Height       int          `json:"height,omitempty"`
	FPSNum       int          `json:"fps_num,omitempty"`
	FPSDen       int          `json:"fps_den,omitempty"`
	FrameCount   int          `json:"frame_count,omitempty"`
	DurationMS   int          `json:"duration_ms,omitempty"`
	Chroma       string       `json:"chroma,omitempty"`
	FlashEventMS int          `json:"flash_event_ms,omitempty"`
	FlashFound   bool         `json:"flash_found"`
	Prominence   float64      `json:"prominence,omitempty"`
	Findings     []Finding    `json:"findings"`
}

// Policy is the analysis policy applied during the health check. Defaults
// match the service defaults in internal/domain.
type Policy struct {
	MinConfidence float64 `json:"min_confidence"`
	MaxOffsetMS   int     `json:"max_offset_ms"`
}

// SyncReport states the analyzable relationship between the two streams.
type SyncReport struct {
	Status          string  `json:"status"`
	AudioEventMS    int     `json:"audio_event_ms,omitempty"`
	VideoEventMS    int     `json:"video_event_ms,omitempty"`
	OffsetMS        int     `json:"offset_ms,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
	DriftPPM        int     `json:"drift_ppm,omitempty"`
	AudioProminence float64 `json:"audio_prominence,omitempty"`
	VideoProminence float64 `json:"video_prominence,omitempty"`
	ErrorCode       string  `json:"error_code,omitempty"`
	ErrorMessage    string  `json:"error_message,omitempty"`
}

// PairReport is one row of the final report: a matched WAV/Y4M pair, an
// unmatched file, or a directory-level pairing conflict.
type PairReport struct {
	Key             string       `json:"key"`
	Directory       string       `json:"directory"`
	Audio           *AudioReport `json:"audio,omitempty"`
	Video           *VideoReport `json:"video,omitempty"`
	Verdict         string       `json:"verdict"`
	ChangedDuringScan bool      `json:"changed_during_scan"`
	RequestBytesEst int64        `json:"base64_request_bytes_estimate"`
	WithinRequestBudget bool     `json:"within_request_budget"`
	Sync            SyncReport   `json:"sync"`
	Findings        []Finding    `json:"findings,omitempty"`
}

// Options configures a scan run.
type Options struct {
	Root              string
	Recursive         bool
	MemoryBudgetBytes int64
	Policy            Policy
	// VerifyContent re-hashes every file after scanning. Stat changes are
	// always detected; this additionally catches same-size rewrites.
	VerifyContent bool
	// scanHook, when non-nil, is invoked once a file stream has been opened
	// and its initial stat captured. It exists for deterministic change
	// detection tests and is not set by the CLI.
	scanHook func(path string)
}

// Report is the complete, machine-readable result of one run.
type Report struct {
	SchemaVersion int         `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at_utc"`
	Tool          string      `json:"tool"`
	Root          string      `json:"scanned_root"`
	Recursive     bool        `json:"recursive"`
	MemoryBudget  int64       `json:"memory_budget_bytes"`
	Policy        Policy      `json:"policy"`
	Totals        Totals      `json:"totals"`
	Pairs         []PairReport `json:"pairs"`
}

// Totals summarizes the batch.
type Totals struct {
	DirectoriesScanned int `json:"directories_scanned"`
	WAVFiles           int `json:"wav_files"`
	Y4MFiles           int `json:"y4m_files"`
	Paired             int `json:"paired_groups"`
	Admittable         int `json:"admittable_pairs"`
	NotAdmittable      int `json:"not_admittable_pairs"`
	Indeterminate      int `json:"indeterminate_groups"`
	SyncReady          int `json:"sync_ready_pairs"`
}
