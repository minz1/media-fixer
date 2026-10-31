package mediaagent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const (
	stuckMinAgeSeconds = 30
	clkTck             = 100
	skippedComm        = "decypharr"
	statStateIdx       = 0
	statStartIdx       = 19
	schedSwitchesIdx   = 2
	syscallFDIdx       = 1
)

func isReadSyscall(nr string) bool {
	switch nr {
	case "0", "17", "19", "295", "327":
		return true
	}
	return false
}

func (o *RealOps) StuckReads() (*mediaagentapi.StuckReadsResult, error) {
	uptimeRaw, err := os.ReadFile(filepath.Join(o.procRoot, "uptime"))
	if err != nil {
		return nil, fmt.Errorf("read uptime: %w", err)
	}
	upField, _, _ := strings.Cut(strings.TrimSpace(string(uptimeRaw)), " ")
	uptime, err := strconv.ParseFloat(upField, 64)
	if err != nil {
		return nil, fmt.Errorf("parse uptime: %w", err)
	}
	if _, err = os.ReadFile(filepath.Join(o.procRoot, "self", "schedstat")); err != nil {
		return nil, fmt.Errorf("schedstat unavailable: %w", err)
	}

	res := &mediaagentapi.StuckReadsResult{Readers: []mediaagentapi.StuckReader{}}
	for _, pid := range numericDirs(o.procRoot) {
		for _, tid := range numericDirs(filepath.Join(o.procRoot, strconv.Itoa(pid), "task")) {
			if r, ok := o.scanTask(pid, tid, uptime); ok {
				res.Readers = append(res.Readers, r)
			}
		}
	}
	return res, nil
}

func numericDirs(dir string) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []int
	for _, e := range entries {
		if n, convErr := strconv.Atoi(e.Name()); convErr == nil && n >= 0 {
			ids = append(ids, n)
		}
	}
	return ids
}

func (o *RealOps) scanTask(pid, tid int, uptime float64) (mediaagentapi.StuckReader, bool) {
	dir := filepath.Join(o.procRoot, strconv.Itoa(pid), "task", strconv.Itoa(tid))
	statRaw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return mediaagentapi.StuckReader{}, false
	}
	comm, state, start, ok := parseStat(string(statRaw))
	if !ok || state != "D" || comm == skippedComm || uptime-float64(start)/clkTck < stuckMinAgeSeconds {
		return mediaagentapi.StuckReader{}, false
	}
	schedRaw, err := os.ReadFile(filepath.Join(dir, "schedstat"))
	if err != nil {
		return mediaagentapi.StuckReader{}, false
	}
	sched := strings.Fields(string(schedRaw))
	if len(sched) <= schedSwitchesIdx {
		return mediaagentapi.StuckReader{}, false
	}
	switches, err := strconv.ParseUint(sched[schedSwitchesIdx], 10, 64)
	if err != nil {
		return mediaagentapi.StuckReader{}, false
	}
	path, via := o.taskFile(pid, dir)
	if path == "" {
		return mediaagentapi.StuckReader{}, false
	}
	return mediaagentapi.StuckReader{
		PID: pid, TID: tid, Start: start, Comm: comm, Switches: switches, Path: path, Via: via,
	}, true
}

func parseStat(s string) (string, string, uint64, bool) {
	open, closeIdx := strings.Index(s, "("), strings.LastIndex(s, ")")
	if open < 0 || closeIdx < open {
		return "", "", 0, false
	}
	fields := strings.Fields(s[closeIdx+1:])
	if len(fields) <= statStartIdx {
		return "", "", 0, false
	}
	start, err := strconv.ParseUint(fields[statStartIdx], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return s[open+1 : closeIdx], fields[statStateIdx], start, true
}

func syscallReadFD(taskDir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(taskDir, "syscall"))
	if err != nil {
		return "", false
	}
	f := strings.Fields(string(raw))
	if len(f) <= syscallFDIdx || !isReadSyscall(f[0]) {
		return "", false
	}
	n, err := strconv.ParseInt(f[syscallFDIdx], 0, 64)
	if err != nil || n < 0 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

func (o *RealOps) taskFile(pid int, taskDir string) (string, string) {
	fdDir := filepath.Join(o.procRoot, strconv.Itoa(pid), "fd")
	if n, ok := syscallReadFD(taskDir); ok {
		if p := o.mediaLink(filepath.Join(fdDir, n)); p != "" {
			return p, "syscall"
		}
		return "", ""
	}
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return "", ""
	}
	var found []string
	for _, e := range entries {
		if p := o.mediaLink(filepath.Join(fdDir, e.Name())); p != "" && !slices.Contains(found, p) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return "", ""
	}
	return found[0], "fd"
}

func (o *RealOps) mediaLink(link string) string {
	target, err := os.Readlink(link)
	if err != nil || !strings.HasPrefix(target, "/") || strings.HasSuffix(target, " (deleted)") {
		return ""
	}
	clean, err := o.resolveUnderMounts(target)
	if err != nil {
		return ""
	}
	return clean
}
