package requesttiming

// RequestPhases are application-observed HTTP phases. Upload starts at the first
// body read (including waiting for the first byte); processing includes routing,
// retries, generation and local response writes after the body is received.
// Neither phase measures time buffered in a reverse proxy or receipt by a client.
type RequestPhases struct {
	UserUploadMS *float64 `json:"user_upload_ms"`
	ProcessingMS *float64 `json:"processing_ms"`
}

func DeriveRequestPhases(s Snapshot) RequestPhases {
	start, started := s.Events["body_read_start"]
	received, complete := s.Events["body_received"]
	_, readFailed := s.Events["body_read_error"]
	if !s.BodyComplete || !started || !complete || readFailed ||
		!finite(start) || !finite(received) || !finite(s.TotalMS) ||
		start < 0 || received < start || s.TotalMS < received {
		return RequestPhases{}
	}
	upload, processing := received-start, s.TotalMS-received
	return RequestPhases{UserUploadMS: &upload, ProcessingMS: &processing}
}
