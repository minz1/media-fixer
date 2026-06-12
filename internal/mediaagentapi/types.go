package mediaagentapi

// DDTestRequest is the body for POST /dd-test.
type DDTestRequest struct {
	Path string `json:"path"`
}

// DDTestResult is the response from POST /dd-test.
type DDTestResult struct {
	BytesRead  int64   `json:"bytes_read"`
	SpeedMBs   float64 `json:"speed_mb_s"`
	Error      string  `json:"error,omitempty"`
}

// RestartRequest is the body for POST /restart/:service.
// (Service name is in the URL path; no body needed — struct kept for future use.)
type RestartResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// DiskMount represents one mount point's usage.
type DiskMount struct {
	Path           string `json:"path"`
	UsedBytes      uint64 `json:"used_bytes"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// DiskResult is the response from GET /disk.
type DiskResult struct {
	Mounts []DiskMount `json:"mounts"`
}

// ErrorResponse is the standard error body for non-2xx responses.
type ErrorResponse struct {
	Error string `json:"error"`
}
