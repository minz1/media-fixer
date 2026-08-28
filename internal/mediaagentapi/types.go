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
	// NotFound is true when Error is specifically "the path does not exist"
	// (ENOENT — on the file itself, or on a parent directory component,
	// which os.IsNotExist reports identically) rather than an I/O error on a
	// file that does exist (EIO, a stale FUSE handle, etc.). The two need
	// completely different fixes: content that was never downloaded is an
	// *arr problem, not a FUSE/decypharr one. Surfaced structurally so the
	// distinction doesn't depend on the model pattern-matching the error
	// string — confirmed necessary live: the Rick and Morty S09E09 incident
	// got exactly this error on both the file and its parent directory and
	// still concluded "FUSE serving stale paths".
	NotFound bool `json:"not_found,omitempty"`
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
