package site

import "time"

// SyncStats counts HTTP body bytes, excluding HTTP/TLS/TCP framing. JSON is
// uncompressed; wire is gzip. Values never include policy or credential data.
type SyncStats struct {
	CompletedAt       time.Time `json:"completed_at"`
	DurationMS        int64     `json:"duration_ms"`
	Full              bool      `json:"full"`
	Rows              int       `json:"rows"`
	Events            int       `json:"events"`
	Pending           int       `json:"pending"`
	UploadJSONBytes   int       `json:"upload_json_bytes"`
	UploadWireBytes   int       `json:"upload_wire_bytes"`
	DownloadJSONBytes int64     `json:"download_json_bytes"`
	DownloadWireBytes int64     `json:"download_wire_bytes"`
}

func (m *Manager) LastSync() (SyncStats, bool) {
	if s := m.lastSync.Load(); s != nil {
		return *s, true
	}
	return SyncStats{}, false
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}
