package mediaagent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func TestRealOps_DiskUsage_AbsentPathNotMounted(t *testing.T) {
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
	if m.Mounted {
		t.Error("expected Mounted=false for non-existent path")
	}
	if m.TotalBytes != 0 || m.UsedBytes != 0 || m.AvailableBytes != 0 {
		t.Errorf("expected zero bytes for non-existent path, got total=%d", m.TotalBytes)
	}
}

func TestRealOps_DiskUsage_PlainDirNotMounted(t *testing.T) {
	t.Parallel()
	// A regular directory that exists but is not a mount point must report Mounted=false.
	// This is the case os.Stat gets wrong — it would return true for any existing path.
	dir := t.TempDir()
	ops := mediaagent.NewRealOps([]string{dir})
	result, err := ops.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("expected 1 mount entry, got %d", len(result.Mounts))
	}
	if result.Mounts[0].Mounted {
		t.Error("expected Mounted=false for a plain directory that is not a mount point")
	}
}

func TestRealOps_DiskUsage_KnownMountIsDetected(t *testing.T) {
	t.Parallel()
	// /proc is always a real mount on Linux; use it as a guaranteed positive case.
	ops := mediaagent.NewRealOps([]string{"/proc"})
	result, err := ops.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("expected 1 mount entry, got %d", len(result.Mounts))
	}
	if !result.Mounts[0].Mounted {
		t.Error("expected Mounted=true for /proc")
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
