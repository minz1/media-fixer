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

// DiskMount reports two orthogonal facts about a configured path plus its byte
// usage. The two booleans are deliberately separate: conflating them into a
// single "mounted" flag is what caused a path to read as healthy when its FUSE
// mount had actually died and fallen back to an empty directory on the root FS.
type DiskMount struct {
	Path string `json:"path"`
	// Accessible is true iff os.Stat on the path succeeds — i.e. the agent can
	// reach it. A cloud-backed FUSE mount that is up reports Accessible=true with
	// zero byte counts (Bsize=0). Accessible does NOT imply a filesystem is mounted:
	// a dead FUSE mount reverts to an empty root-FS directory that still stats fine.
	Accessible bool `json:"accessible"`
	// IsMountPoint is true iff the path appears as a mount target in
	// /proc/self/mountinfo — i.e. a filesystem is actually mounted there. For
	// /mnt/decypharr this is the authoritative "is the FUSE mount live" signal;
	// for a plain directory on the root FS (e.g. /data) it is legitimately false.
	IsMountPoint   bool   `json:"is_mount_point"`
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
