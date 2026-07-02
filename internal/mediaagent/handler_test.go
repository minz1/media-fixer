package mediaagent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func TestRealOps_DiskUsage_AbsentPath(t *testing.T) {
	t.Parallel()
	ops := mediaagent.NewRealOps([]string{"/nonexistent/path/that/cannot/exist"})
	result, err := ops.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("expected 1 mount entry, got %d", len(result.Mounts))
	}
	m := result.Mounts[0]
	if m.Accessible {
		t.Error("expected Accessible=false for non-existent path")
	}
	if m.IsMountPoint {
		t.Error("expected IsMountPoint=false for non-existent path")
	}
	if m.TotalBytes != 0 || m.UsedBytes != 0 || m.AvailableBytes != 0 {
		t.Errorf("expected zero bytes for non-existent path, got total=%d", m.TotalBytes)
	}
}

func TestRealOps_DiskUsage_PlainDirAccessibleButNotMountPoint(t *testing.T) {
	t.Parallel()
	// A plain directory on the root FS is the /data case that bug 4 got wrong:
	// it is Accessible with real byte counts, but it is NOT a mount point. Both
	// signals must reflect that independently.
	dir := t.TempDir()
	ops := mediaagent.NewRealOps([]string{dir})
	result, err := ops.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("expected 1 mount entry, got %d", len(result.Mounts))
	}
	m := result.Mounts[0]
	if !m.Accessible {
		t.Error("expected Accessible=true for a real directory")
	}
	if m.IsMountPoint {
		t.Error("expected IsMountPoint=false for a plain dir that is not a mount target")
	}
	if m.TotalBytes == 0 {
		t.Error("expected non-zero TotalBytes for a real filesystem path")
	}
}

func TestRealOps_DiskUsage_KnownKernelMountIsMountPoint(t *testing.T) {
	t.Parallel()
	// /proc is always a mount point on Linux — pins the IsMountPoint signal itself.
	ops := mediaagent.NewRealOps([]string{"/proc"})
	result, err := ops.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("expected 1 mount entry, got %d", len(result.Mounts))
	}
	m := result.Mounts[0]
	if !m.Accessible {
		t.Error("expected Accessible=true for /proc")
	}
	if !m.IsMountPoint {
		t.Error("expected IsMountPoint=true for /proc")
	}
}

// listDirByName runs ListDir on root and returns its entries keyed by name.
func listDirByName(t *testing.T, root string) map[string]mediaagentapi.ListDirEntry {
	t.Helper()
	ops := mediaagent.NewRealOps([]string{root})
	result, err := ops.ListDir(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]mediaagentapi.ListDirEntry, len(result.Entries))
	for _, e := range result.Entries {
		byName[e.Name] = e
	}
	return byName
}

func TestRealOps_ListDir_ReportsSymlinkTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := "/mnt/decypharr/__all__/Some.Torrent/video.mkv"
	if err := os.Symlink(target, filepath.Join(root, "episode.mkv")); err != nil {
		t.Fatal(err)
	}

	link := listDirByName(t, root)["episode.mkv"]
	if !link.IsSymlink {
		t.Error("expected episode.mkv to be a symlink")
	}
	if link.Target != target {
		t.Errorf("target: got %q want %q", link.Target, target)
	}
}

func TestRealOps_ListDir_RegularFileNotSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := listDirByName(t, root)["regular.txt"]
	if reg.IsSymlink {
		t.Error("regular.txt should not be a symlink")
	}
	if reg.Target != "" {
		t.Errorf("regular file should have no target, got %q", reg.Target)
	}
}
