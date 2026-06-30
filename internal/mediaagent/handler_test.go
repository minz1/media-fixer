package mediaagent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

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
