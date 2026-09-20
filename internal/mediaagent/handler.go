package mediaagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// Ops is the interface for OS-level operations; swapped for stubs in tests.
type Ops interface {
	DDTest(ctx context.Context, path string) (*mediaagentapi.DDTestResult, error)
	Restart(ctx context.Context, service string) error
	DiskUsage() (*mediaagentapi.DiskResult, error)
	ListDir(path string) (*mediaagentapi.ListDirResult, error)
}

// NewHandler builds the media-agent HTTP router.
func NewHandler(ops Ops, apiKey string, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(bearerAuth(apiKey))

	r.Post("/restart/{service}", func(w http.ResponseWriter, req *http.Request) {
		svc := chi.URLParam(req, "service")
		if err := ops.Restart(req.Context(), svc); err != nil {
			log.ErrorContext(req.Context(), "restart failed", "service", svc, "error", err)
			writeJSON(w, http.StatusInternalServerError, mediaagentapi.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, mediaagentapi.RestartResult{Status: "ok"})
	})

	r.Post("/dd-test", func(w http.ResponseWriter, req *http.Request) {
		var body mediaagentapi.DDTestRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Path == "" {
			writeJSON(w, http.StatusBadRequest, mediaagentapi.ErrorResponse{Error: "path required"})
			return
		}
		result, err := ops.DDTest(req.Context(), body.Path)
		if err != nil {
			log.ErrorContext(req.Context(), "dd-test failed", "path", body.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, mediaagentapi.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	r.Get("/ls", func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Query().Get("path")
		if path == "" {
			writeJSON(w, http.StatusBadRequest, mediaagentapi.ErrorResponse{Error: "path required"})
			return
		}
		result, err := ops.ListDir(path)
		if err != nil {
			log.ErrorContext(req.Context(), "ls failed", "path", path, "error", err)
			writeJSON(w, http.StatusInternalServerError, mediaagentapi.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	r.Get("/disk", func(w http.ResponseWriter, req *http.Request) {
		result, err := ops.DiskUsage()
		if err != nil {
			log.ErrorContext(req.Context(), "disk usage failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, mediaagentapi.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	return r
}

func bearerAuth(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			// The ok result matters: discarding it meant a bare
			// "Authorization: <token>" with no scheme authenticated just as
			// well as a correctly-formed bearer header.
			token, ok := strings.CutPrefix(auth, "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
				writeJSON(w, http.StatusUnauthorized, mediaagentapi.ErrorResponse{Error: "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RealOps executes actual OS operations.
type RealOps struct {
	mounts []string
}

// defaultMounts are the paths checked when no explicit mounts are provided.
func defaultMounts() []string {
	return []string{"/mnt/decypharr", "/var/cache/decypharr", "/data"}
}

// NewRealOps creates a RealOps with the given mount paths.
// If mounts is empty, sensible defaults are used.
func NewRealOps(mounts []string) *RealOps {
	if len(mounts) == 0 {
		mounts = defaultMounts()
	}
	return &RealOps{mounts: mounts}
}

// ddBlockSize × ddCount = ~100 MiB read.
const (
	ddBlockSize = 4096
	ddCount     = 25600
	bytesPerMiB = 1024 * 1024
)

func (o *RealOps) DDTest(ctx context.Context, path string) (*mediaagentapi.DDTestResult, error) {
	// Same allowlist as ListDir: this one had none at all, so an
	// LLM-supplied path made it an arbitrary-file readability oracle for the
	// whole filesystem.
	path, mountErr := o.resolveUnderMounts(path)
	if mountErr != nil {
		return nil, mountErr
	}

	info, err := os.Stat(path) //nolint:gosec // G703: path is validated by resolveUnderMounts above
	if err != nil {
		return &mediaagentapi.DDTestResult{Error: err.Error(), NotFound: os.IsNotExist(err)}, nil
	}
	if info.IsDir() {
		return &mediaagentapi.DDTestResult{
			Error: "path is a directory, not a file — use list_directory to find the specific video file inside it",
		}, nil
	}
	// Anything that is not a regular file (a FIFO, a device node) can block
	// forever in os.Open itself, before any read deadline could apply.
	if !info.Mode().IsRegular() {
		return &mediaagentapi.DDTestResult{
			Error: "path is not a regular file",
		}, nil
	}

	f, err := os.Open(path) //nolint:gosec // G703: path is validated by resolveUnderMounts above
	if err != nil {
		return &mediaagentapi.DDTestResult{Error: err.Error(), NotFound: os.IsNotExist(err)}, nil
	}
	defer f.Close()

	buf := make([]byte, ddBlockSize)
	var total int64
	start := time.Now()

	for range ddCount {
		// A hung FUSE mount is exactly what this tool exists to diagnose, and
		// reads against one block in uninterruptible I/O. Checking the request
		// context each block means a client disconnect (or the client's own
		// timeout) ends the loop instead of parking the goroutine forever.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &mediaagentapi.DDTestResult{BytesRead: total, Error: ctxErr.Error()}, nil
		}
		n, readErr := f.Read(buf)
		total += int64(n)
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return &mediaagentapi.DDTestResult{BytesRead: total, Error: readErr.Error()}, nil
		}
	}

	elapsed := time.Since(start).Seconds()
	var speed float64
	if elapsed > 0 {
		speed = float64(total) / elapsed / bytesPerMiB
	}
	return &mediaagentapi.DDTestResult{BytesRead: total, SpeedMBs: speed}, nil
}

func (o *RealOps) Restart(ctx context.Context, service string) error {
	switch service {
	case "jellyfin":
		out, err := exec.CommandContext(ctx, "systemctl", "restart", "jellyfin").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl restart jellyfin: %w — %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "decypharr":
		out, err := exec.CommandContext(ctx, "systemctl", "restart", "decypharr").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl restart decypharr: %w — %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("unknown service %q", service)
	}
}

// resolveUnderMounts cleans path and returns it only if it resolves inside one
// of the configured mount roots.
//
// The cleaning is the whole point: the previous check ran [strings.HasPrefix] on
// the raw request string, so "/mnt/decypharr/../../etc" carried an allowed
// prefix, passed, and was then handed to [os.ReadDir], which resolves it — a
// full listing of any directory on the media host. Paths here come from LLM
// output, so this is directly model-steerable.
//
// Shared by every path-taking operation rather than duplicated per call site,
// so a new one cannot be added without it.
func (o *RealOps) resolveUnderMounts(path string) (string, error) {
	clean := filepath.Clean(path)
	for _, root := range o.mounts {
		cleanRoot := filepath.Clean(root)
		if clean == cleanRoot || strings.HasPrefix(clean, cleanRoot+string(filepath.Separator)) {
			return clean, nil
		}
	}
	return "", fmt.Errorf("path %q is outside allowed mount roots", path)
}

func (o *RealOps) ListDir(path string) (*mediaagentapi.ListDirResult, error) {
	path, err := o.resolveUnderMounts(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := &mediaagentapi.ListDirResult{Path: path}
	for _, e := range entries {
		entry := mediaagentapi.ListDirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if e.Type()&os.ModeSymlink != 0 {
			entry.IsSymlink = true
			if target, linkErr := os.Readlink(filepath.Join(path, e.Name())); linkErr == nil {
				entry.Target = target
			}
		}
		if !e.IsDir() {
			if info, infoErr := e.Info(); infoErr == nil {
				entry.Size = info.Size()
			}
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

// mountInfoTargetField is the 0-based index of the mount target path in
// /proc/self/mountinfo lines: "mountID parentID major:minor root mountTarget ...".
const mountInfoTargetField = 4

// activeMountPoints parses /proc/self/mountinfo and returns the set of active
// mount targets. Used to compute DiskMount.IsMountPoint.
func activeMountPoints() map[string]struct{} {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	set := make(map[string]struct{})
	for line := range strings.SplitSeq(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > mountInfoTargetField {
			set[fields[mountInfoTargetField]] = struct{}{}
		}
	}
	return set
}

func (o *RealOps) DiskUsage() (*mediaagentapi.DiskResult, error) {
	mountPoints := activeMountPoints()
	mounts := make([]mediaagentapi.DiskMount, 0, len(o.mounts))
	for _, path := range o.mounts {
		// Two orthogonal facts. Accessible: can the agent reach the path (os.Stat).
		// IsMountPoint: is a filesystem actually mounted there (mountinfo). A dead
		// FUSE mount reverts to an empty root-FS dir → Accessible=true but
		// IsMountPoint=false, which is the only way to tell it is really down.
		_, statErr := os.Stat(path)
		_, isMountPoint := mountPoints[path]
		entry := mediaagentapi.DiskMount{
			Path:         path,
			Accessible:   statErr == nil,
			IsMountPoint: isMountPoint,
		}

		// Statfs for byte counts; skip silently if Bsize==0 (cloud-backed, no blocks).
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil && stat.Bsize > 0 {
			bsize := uint64(stat.Bsize)
			entry.TotalBytes = stat.Blocks * bsize
			entry.AvailableBytes = stat.Bavail * bsize
			// Bfree, not Bavail: Bavail excludes the root-reserved blocks
			// (typically 5%), so using it overstated usage by the reserve —
			// ~50 GB of phantom "used" space on a 1 TB filesystem, fed
			// straight to an LLM asked whether the disk is full.
			entry.UsedBytes = (stat.Blocks - stat.Bfree) * bsize
		}

		mounts = append(mounts, entry)
	}
	return &mediaagentapi.DiskResult{Mounts: mounts}, nil
}
