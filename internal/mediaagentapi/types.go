package mediaagentapi

// DDTestRequest is the body for POST /dd-test.
type DDTestRequest struct {
	Path string `json:"path"`
}

// DDTestResult is the response from POST /dd-test.
type DDTestResult struct {
	BytesRead int64   `json:"bytes_read"`
	SpeedMBs  float64 `json:"speed_mb_s"`
	Error     string  `json:"error,omitempty"`
}

// RestartResult is the response from POST /restart/:service.
type RestartResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// DiskMount represents one mount point's usage.
type DiskMount struct {
	Path string `json:"path"`
	// Mounted is false when os.Stat on the path fails — the mount is absent or inaccessible.
	// Cloud-backed FUSE mounts (e.g. decypharr) will have Mounted=true with zero byte counts.
	Mounted        bool   `json:"mounted"`
	UsedBytes      uint64 `json:"used_bytes"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// DiskResult is the response from GET /disk.
type DiskResult struct {
	Mounts []DiskMount `json:"mounts"`
}

// ListDirEntry is one item returned by GET /ls.
type ListDirEntry struct {
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	Size      int64  `json:"size,omitempty"`       // bytes; 0 for directories
	IsSymlink bool   `json:"is_symlink,omitempty"` // true if the entry is a symlink
	Target    string `json:"target,omitempty"`     // symlink target (e.g. into /mnt/decypharr/__all__/...)
}

// ListDirResult is the response from GET /ls.
type ListDirResult struct {
	Path    string         `json:"path"`
	Entries []ListDirEntry `json:"entries"`
}

// ErrorResponse is the standard error body for non-2xx responses.
type ErrorResponse struct {
	Error string `json:"error"`
}
