package usagestats

// RequestPhaseAverages includes only requests with a single retained trace and
// complete HTTP body boundaries. Unknown observations never become zero samples.
type RequestPhaseAverages struct {
	AverageUserUploadMs *float64 `json:"average_user_upload_ms"`
	AverageProcessingMs *float64 `json:"average_processing_ms"`
	TimingSampleCount   int64    `json:"timing_sample_count"`
}
