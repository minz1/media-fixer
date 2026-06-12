package mediaagent

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// Ops is the interface for OS-level operations; swapped for stubs in tests.
type Ops interface {
	DDTest(path string) (*mediaagentapi.DDTestResult, error)
	Restart(service string) error
	DiskUsage() (*mediaagentapi.DiskResult, error)
}

// NewHandler builds the media-agent HTTP router.
func NewHandler(ops Ops, apiKey string, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(bearerAuth(apiKey))

	r.Post("/restart/{service}", func(w http.ResponseWriter, req *http.Request) {
		svc := chi.URLParam(req, "service")
		if err := ops.Restart(svc); err != nil {
			log.Error("restart failed", "service", svc, "error", err)
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
		result, err := ops.DDTest(body.Path)
		if err != nil {
			log.Error("dd-test failed", "path", body.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, mediaagentapi.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	r.Get("/disk", func(w http.ResponseWriter, req *http.Request) {
		result, err := ops.DiskUsage()
		if err != nil {
			log.Error("disk usage failed", "error", err)
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
			token, _ := strings.CutPrefix(auth, "Bearer ")
			if token != key {
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
type RealOps struct{}

// ddBlockSize × ddCount = ~100 MB read.
const (
	ddBlockSize = 4096
	ddCount     = 25600
)

func (o *RealOps) DDTest(path string) (*mediaagentapi.DDTestResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return &mediaagentapi.DDTestResult{Error: err.Error()}, nil
	}
	defer f.Close()

	buf := make([]byte, ddBlockSize)
	var total int64
	start := time.Now()

	for i := 0; i < ddCount; i++ {
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
		speed = float64(total) / elapsed / (1024 * 1024)
	}
	return &mediaagentapi.DDTestResult{BytesRead: total, SpeedMBs: speed}, nil
}

func (o *RealOps) Restart(service string) error {
	out, err := exec.Command("systemctl", "restart", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %w — %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DiskMounts are the paths checked for disk usage.
var DiskMounts = []string{"/mnt/decypharr", "/var/cache/decypharr", "/data"}

func (o *RealOps) DiskUsage() (*mediaagentapi.DiskResult, error) {
	var mounts []mediaagentapi.DiskMount
	for _, path := range DiskMounts {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err != nil {
			continue
		}
		total := stat.Blocks * uint64(stat.Bsize)
		avail := stat.Bavail * uint64(stat.Bsize)
		mounts = append(mounts, mediaagentapi.DiskMount{
			Path:           path,
			TotalBytes:     total,
			AvailableBytes: avail,
			UsedBytes:      total - avail,
		})
	}
	return &mediaagentapi.DiskResult{Mounts: mounts}, nil
}
