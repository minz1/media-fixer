package mediaagent_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/mediaagent"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fakeProc(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	writeFile(t, filepath.Join(proc, "uptime"), "1000.00 5.00\n")
	writeFile(t, filepath.Join(proc, "self", "schedstat"), "1 2 3\n")
	return proc, filepath.Join(root, "mnt")
}

func statLine(tid int, comm, state string, start uint64) string {
	f := strings.Fields(strings.Repeat("0 ", 20))
	f[0] = state
	f[19] = strconv.FormatUint(start, 10)
	return fmt.Sprintf("%d (%s) %s\n", tid, comm, strings.Join(f, " "))
}

func task(t *testing.T, proc string, pid, tid int, comm, state string, start, switches uint64, syscall string) {
	t.Helper()
	dir := filepath.Join(proc, strconv.Itoa(pid), "task", strconv.Itoa(tid))
	writeFile(t, filepath.Join(dir, "stat"), statLine(tid, comm, state, start))
	writeFile(t, filepath.Join(dir, "schedstat"), fmt.Sprintf("10 20 %d\n", switches))
	if syscall != "" {
		writeFile(t, filepath.Join(dir, "syscall"), syscall+"\n")
	}
}

func fd(t *testing.T, proc string, pid, n int, target string) {
	t.Helper()
	dir := filepath.Join(proc, strconv.Itoa(pid), "fd")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, strconv.Itoa(n))); err != nil {
		t.Fatal(err)
	}
}

func TestStuckReadsThreeOnOneFile(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	target := mount + "/__all__/T/e.mkv"
	for i := range 3 {
		task(t, proc, 10, 10+i, "Thread", "D", 1000, uint64(5+i), "0 0x3 0x0 0x0 0x0 0x0 0x0 0x1 0x2")
	}
	fd(t, proc, 10, 3, target)
	res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 3 {
		t.Fatalf("readers = %d, want 3: %+v", len(res.Readers), res.Readers)
	}
	for i, r := range res.Readers {
		if r.Via != "syscall" || r.Path != target || r.Switches != uint64(5+i) || r.PID != 10 || r.Start != 1000 {
			t.Errorf("reader %d = %+v", i, r)
		}
	}
}

func TestStuckReadsYoungAndSleeping(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	task(t, proc, 10, 10, "t", "D", 99000, 1, "0 0x3 0x0")
	task(t, proc, 11, 11, "t", "S", 1000, 1, "0 0x3 0x0")
	fd(t, proc, 10, 3, mount+"/a.mkv")
	fd(t, proc, 11, 3, mount+"/a.mkv")
	res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 0 {
		t.Errorf("readers = %+v, want none", res.Readers)
	}
}

func TestStuckReadsRejectsNonMedia(t *testing.T) {
	t.Parallel()
	for i, target := range []string{"/etc/passwd", "socket:[123]", "MOUNT/x.mkv (deleted)"} {
		proc, mount := fakeProc(t)
		task(t, proc, 10, 10, "t", "D", 1000, 1, "0 0x3 0x0")
		fd(t, proc, 10, 3, strings.Replace(target, "MOUNT", mount, 1))
		res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Readers) != 0 {
			t.Errorf("case %d %q: readers = %+v, want none", i, target, res.Readers)
		}
	}
}

func TestStuckReadsSyscalls(t *testing.T) {
	t.Parallel()
	cases := []struct {
		syscall string
		want    int
		via     string
	}{
		{"17 0x3 0x0 0x0", 1, "syscall"},
		{"327 0x3 0x0 0x0", 1, "syscall"},
		{"running", 1, "fd"},
		{"-1 0x0 0x0", 1, "fd"},
		{"257 0x3 0x0 0x0", 1, "fd"},
		{"", 1, "fd"},
	}
	for _, tc := range cases {
		proc, mount := fakeProc(t)
		task(t, proc, 10, 10, "t", "D", 1000, 1, tc.syscall)
		fd(t, proc, 10, 3, mount+"/a.mkv")
		res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Readers) != tc.want || res.Readers[0].Via != tc.via {
			t.Errorf("syscall %q: readers = %+v, want %d via %s", tc.syscall, res.Readers, tc.want, tc.via)
		}
	}
}

func TestStuckReadsFDFallback(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	task(t, proc, 10, 10, "t", "D", 1000, 1, "running")
	fd(t, proc, 10, 0, "/dev/null")
	fd(t, proc, 10, 4, "socket:[9]")
	fd(t, proc, 10, 7, mount+"/a.mkv")
	ops := mediaagent.NewProcOps(proc, []string{mount})
	res, err := ops.StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 1 || res.Readers[0].Via != "fd" || res.Readers[0].Path != mount+"/a.mkv" {
		t.Fatalf("readers = %+v", res.Readers)
	}
	fd(t, proc, 10, 8, mount+"/b.mkv")
	res, err = ops.StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 0 {
		t.Errorf("two media fds: readers = %+v, want none", res.Readers)
	}
}

func TestStuckReadsTaskGone(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	task(t, proc, 10, 10, "t", "D", 1000, 1, "0 0x3 0x0")
	gone := filepath.Join(proc, "10", "task", "11")
	writeFile(t, filepath.Join(gone, "stat"), statLine(11, "t", "D", 1000))
	fd(t, proc, 10, 3, mount+"/a.mkv")
	res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 1 || res.Readers[0].TID != 10 {
		t.Errorf("readers = %+v", res.Readers)
	}
}

func TestStuckReadsCommParsing(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	task(t, proc, 10, 10, "decypharr", "D", 1000, 1, "0 0x3 0x0")
	task(t, proc, 11, 11, "a) b c", "D", 1000, 1, "0 0x3 0x0")
	fd(t, proc, 10, 3, mount+"/a.mkv")
	fd(t, proc, 11, 3, mount+"/a.mkv")
	res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 1 || res.Readers[0].PID != 11 || res.Readers[0].Comm != "a) b c" {
		t.Errorf("readers = %+v", res.Readers)
	}
}

func TestStuckReadsNoSchedstat(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	if err := os.Remove(filepath.Join(proc, "self", "schedstat")); err != nil {
		t.Fatal(err)
	}
	if _, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads(); err == nil {
		t.Error("want error without self/schedstat")
	}
}

func TestStuckReadsEmptyUptime(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	writeFile(t, filepath.Join(proc, "uptime"), "")
	if _, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads(); err == nil {
		t.Error("want error for an empty uptime")
	}
}

func TestStuckReadsNeverStats(t *testing.T) {
	t.Parallel()
	proc, mount := fakeProc(t)
	target := mount + "/a.mkv"
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target must not exist: %v", err)
	}
	task(t, proc, 10, 10, "t", "D", 1000, 1, "0 0x3 0x0")
	fd(t, proc, 10, 3, target)
	res, err := mediaagent.NewProcOps(proc, []string{mount}).StuckReads()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 1 || res.Readers[0].Path != target {
		t.Errorf("readers = %+v", res.Readers)
	}
}
