package client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// MediaHostClient runs commands on minz-media-0 over SSH/WireGuard for
// diagnostics (dd readability) and service restarts that can't be triggered
// via a remote API.
type MediaHostClient struct {
	host    string
	port    int
	user    string
	signer  ssh.Signer
}

func NewMediaHost(host string, port int, user, sshKeyPath string) (*MediaHostClient, error) {
	keyBytes, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", sshKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	if port == 0 {
		port = 22
	}
	return &MediaHostClient{host: host, port: port, user: user, signer: signer}, nil
}

type DDResult struct {
	BytesRead int64
	Speed     string
	Error     string
}

// DDReadabilityTest runs a non-destructive dd read on the given file path and
// returns how many bytes were read before failure (or success) and the speed.
func (m *MediaHostClient) DDReadabilityTest(ctx context.Context, filePath string) (*DDResult, error) {
	// Read the first 100 MB in 4 KB blocks; time limit via SSH deadline.
	cmd := fmt.Sprintf(
		"dd if=%s bs=4K count=25600 of=/dev/null 2>&1; echo exit:$?",
		shellEscape(filePath),
	)
	out, err := m.run(ctx, cmd)
	result := parseDDOutput(out)
	if err != nil {
		result.Error = err.Error()
	}
	return result, nil
}

// RestartService restarts a systemd service on the media host.
func (m *MediaHostClient) RestartService(ctx context.Context, service string) error {
	_, err := m.run(ctx, fmt.Sprintf("systemctl restart %s", shellEscape(service)))
	return err
}

func (m *MediaHostClient) run(ctx context.Context, cmd string) (string, error) {
	cfg := &ssh.ClientConfig{
		User:            m.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(m.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // acceptable for WireGuard peer
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", m.host, m.port)
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	// Propagate context cancellation via a deadline on the underlying net.Conn.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.Conn.(net.Conn).SetDeadline(deadline)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	var buf bytes.Buffer
	sess.Stdout = &buf
	sess.Stderr = &buf
	if err := sess.Run(cmd); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func parseDDOutput(out string) *DDResult {
	r := &DDResult{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "bytes") && strings.Contains(line, "copied") {
			// e.g. "104857600 bytes (105 MB, 100 MiB) copied, 2.5 s, 41.9 MB/s"
			var bytes int64
			fmt.Sscanf(line, "%d bytes", &bytes)
			r.BytesRead = bytes
			if idx := strings.LastIndex(line, ","); idx >= 0 {
				r.Speed = strings.TrimSpace(line[idx+1:])
			}
		}
		if strings.HasPrefix(line, "exit:") {
			code := strings.TrimPrefix(line, "exit:")
			if code != "0" {
				r.Error = fmt.Sprintf("dd exited with code %s", code)
			}
		}
	}
	return r
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
