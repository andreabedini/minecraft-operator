package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

const (
	progressInterval = 500 * time.Millisecond
	downloadChunk    = 256 * 1024
)

// checkDownloadURL enforces the scheme and host policy.
func (s *Server) checkDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && !(s.cfg.AllowInsecureDownloads && u.Scheme == "http") {
		return nil, fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("url has no host")
	}
	if len(s.cfg.DownloadAllowHosts) > 0 {
		host := strings.ToLower(u.Hostname())
		ok := false
		for _, allowed := range s.cfg.DownloadAllowHosts {
			allowed = strings.ToLower(allowed)
			if host == allowed || (strings.HasPrefix(allowed, "*.") && strings.HasSuffix(host, allowed[1:])) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("host %q not in allowlist", host)
		}
	}
	return u, nil
}

// download fetches url into the API path, verifying the expected digest,
// and reports progress through send.
func (s *Server) download(ctx context.Context, req *supervisorv1.DownloadRequest, send func(*supervisorv1.DownloadResponse) error) (*supervisorv1.DownloadResult, error) {
	u, err := s.checkDownloadURL(req.GetUrl())
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	dest, err := s.destination(req.GetPath(), false)
	if err != nil {
		return nil, fmt.Errorf("path: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", "minecraft-operator-supervisor/"+s.cfg.Version)
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: %s", u.Redacted(), resp.Status)
	}
	total := resp.ContentLength

	var (
		last    time.Time
		sendErr error
	)
	progress := func(n int64, force bool) {
		if sendErr != nil {
			return
		}
		if !force && time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		sendErr = send(&supervisorv1.DownloadResponse{Event: &supervisorv1.DownloadResponse_Progress{
			Progress: &supervisorv1.DownloadProgress{Bytes: uint64(max(n, 0)), Total: total},
		}})
	}

	buf := make([]byte, downloadChunk)
	var read int64
	result, err := receiveToFile(dest, fileMode(req.GetFileMode(), 0o644), func() ([]byte, error) {
		if sendErr != nil {
			return nil, sendErr
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			read += int64(n)
			progress(read, false)
			return buf[:n], nil
		}
		if err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	progress(read, true)
	if sendErr != nil {
		_ = os.Remove(dest)
		return nil, sendErr
	}
	if err := checkDigest(req.GetExpected(), result.Digests); err != nil {
		_ = os.Remove(dest)
		return nil, err
	}
	rel, _ := s.root.Rel(dest)
	if err := s.state.RecordFile(rel, ManifestEntry{
		Size:    result.Size,
		Digests: digestsToMap(result.Digests),
		Source:  u.Redacted(),
		Time:    time.Now(),
	}); err != nil {
		return nil, err
	}
	s.logger.Info("downloaded", "url", u.Redacted(), "path", rel, "size", result.Size)
	return &supervisorv1.DownloadResult{Size: uint64(max(result.Size, 0)), Digests: result.Digests}, nil
}
