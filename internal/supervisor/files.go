package supervisor

import (
	"crypto/sha1" //nolint:gosec // upstream publishes sha1 for some artifacts
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

// multiHasher computes sha1, sha256 and sha512 in one pass.
type multiHasher struct {
	sha1   hash.Hash
	sha256 hash.Hash
	sha512 hash.Hash
	w      io.Writer
	n      int64
}

func newMultiHasher() *multiHasher {
	h := &multiHasher{sha1: sha1.New(), sha256: sha256.New(), sha512: sha512.New()} //nolint:gosec
	h.w = io.MultiWriter(h.sha1, h.sha256, h.sha512)
	return h
}

func (h *multiHasher) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.n += int64(n)
	return n, err
}

func (h *multiHasher) Digests() []*supervisorv1.Digest {
	return []*supervisorv1.Digest{
		{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1, Hex: hex.EncodeToString(h.sha1.Sum(nil))},
		{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: hex.EncodeToString(h.sha256.Sum(nil))},
		{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA512, Hex: hex.EncodeToString(h.sha512.Sum(nil))},
	}
}

func (h *multiHasher) Get(algo supervisorv1.DigestAlgorithm) string {
	for _, d := range h.Digests() {
		if d.Algorithm == algo {
			return d.Hex
		}
	}
	return ""
}

func digestsToMap(ds []*supervisorv1.Digest) map[string]string {
	out := make(map[string]string, len(ds))
	for _, d := range ds {
		out[algoName(d.GetAlgorithm())] = d.GetHex()
	}
	return out
}

func algoName(a supervisorv1.DigestAlgorithm) string {
	switch a {
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1:
		return "sha1"
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256:
		return "sha256"
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA512:
		return "sha512"
	default:
		return "unspecified"
	}
}

// checkDigest compares an expected digest against observed ones.
func checkDigest(expected *supervisorv1.Digest, observed []*supervisorv1.Digest) error {
	if expected == nil || expected.GetAlgorithm() == supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_UNSPECIFIED {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(expected.GetHex()))
	for _, d := range observed {
		if d.GetAlgorithm() == expected.GetAlgorithm() {
			if d.GetHex() == want {
				return nil
			}
			return fmt.Errorf("%s mismatch: expected %s, got %s", algoName(expected.GetAlgorithm()), want, d.GetHex())
		}
	}
	return fmt.Errorf("unsupported digest algorithm %s", expected.GetAlgorithm())
}

// digestFile hashes a file with one algorithm.
func digestFile(path string, algo supervisorv1.DigestAlgorithm) (*supervisorv1.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var h hash.Hash
	switch algo {
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1:
		h = sha1.New() //nolint:gosec
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256:
		h = sha256.New()
	case supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA512:
		h = sha512.New()
	default:
		return nil, fmt.Errorf("unsupported digest algorithm %s", algo)
	}
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return &supervisorv1.Digest{Algorithm: algo, Hex: hex.EncodeToString(h.Sum(nil))}, nil
}

func fileInfoOf(rel string, fi fs.FileInfo) *supervisorv1.FileInfo {
	return &supervisorv1.FileInfo{
		Path:       filepath.ToSlash(rel),
		Size:       uint64(max(fi.Size(), 0)),
		ModifiedAt: timestamppb.New(fi.ModTime()),
		FileMode:   uint32(fi.Mode().Perm()),
		IsDir:      fi.IsDir(),
		IsSymlink:  fi.Mode()&os.ModeSymlink != 0,
	}
}

// listFiles lists an API path. Directories return their entries; a file
// returns itself. The state directory is skipped.
func (s *Server) listFiles(rel string, recursive bool, algo supervisorv1.DigestAlgorithm) ([]*supervisorv1.FileInfo, error) {
	abs, err := s.root.ResolveUser(rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	var out []*supervisorv1.FileInfo
	add := func(path string, fi fs.FileInfo) error {
		r, err := s.root.Rel(path)
		if err != nil {
			return err
		}
		info := fileInfoOf(r, fi)
		if algo != supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_UNSPECIFIED && fi.Mode().IsRegular() {
			d, err := digestFile(path, algo)
			if err != nil {
				return err
			}
			info.Digest = d
		}
		out = append(out, info)
		return nil
	}
	if !fi.IsDir() {
		if err := add(abs, fi); err != nil {
			return nil, err
		}
		return out, nil
	}
	if !recursive {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if abs == s.root.Dir && e.Name() == stateDirName {
				continue
			}
			efi, err := e.Info()
			if err != nil {
				return nil, err
			}
			if err := add(filepath.Join(abs, e.Name()), efi); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == abs {
			return nil
		}
		if d.IsDir() && path == s.root.StateDir() {
			return filepath.SkipDir
		}
		dfi, err := d.Info()
		if err != nil {
			return err
		}
		return add(path, dfi)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// destination returns where a write for rel lands: the staged mirror or the
// real tree.
func (s *Server) destination(rel string, staged bool) (string, error) {
	clean, err := s.root.Clean(rel)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return "", errors.New("path must name a file")
	}
	if _, err := s.root.ResolveUser(clean); err != nil {
		return "", err
	}
	if staged {
		return filepath.Join(s.state.StagedDir(), clean), nil
	}
	return s.root.ResolveUser(clean)
}

// writeResult is what a completed file write reports.
type writeResult struct {
	Size    int64
	Digests []*supervisorv1.Digest
}

// receiveToFile streams chunks into a temporary file and renames it over dest.
func receiveToFile(dest string, mode os.FileMode, next func() ([]byte, error)) (*writeResult, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	fail := func(err error) (*writeResult, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return nil, err
	}
	h := newMultiHasher()
	w := io.MultiWriter(tmp, h)
	for {
		chunk, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if _, err := w.Write(chunk); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return nil, err
	}
	return &writeResult{Size: h.n, Digests: h.Digests()}, nil
}

func fileMode(bits uint32, def os.FileMode) os.FileMode {
	if bits == 0 {
		return def
	}
	return os.FileMode(bits) & os.ModePerm
}

// stagedCount counts pending staged writes and deletes.
func (s *Server) stagedCount() int {
	n := 0
	_ = filepath.WalkDir(s.state.StagedDir(), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	dels, _ := s.state.LoadStagedDeletes()
	return n + len(dels)
}

// applyStaged moves every staged file into place and performs staged
// deletes. It returns the API paths written and deleted.
func (s *Server) applyStaged() (written, deleted []string, err error) {
	stagedDir := s.state.StagedDir()
	var files []string
	err = filepath.WalkDir(stagedDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(files)
	manifest, err := s.state.LoadManifest()
	if err != nil {
		return nil, nil, err
	}
	for _, src := range files {
		rel, err := filepath.Rel(stagedDir, src)
		if err != nil {
			return written, deleted, err
		}
		dest, err := s.root.ResolveUser(rel)
		if err != nil {
			return written, deleted, fmt.Errorf("staged %s: %w", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return written, deleted, err
		}
		if err := os.Rename(src, dest); err != nil {
			return written, deleted, err
		}
		fi, err := os.Stat(dest)
		if err == nil {
			h := newMultiHasher()
			if f, err := os.Open(dest); err == nil {
				_, _ = io.Copy(h, f)
				_ = f.Close()
			}
			manifest.Files[filepath.ToSlash(rel)] = ManifestEntry{Size: fi.Size(), Digests: digestsToMap(h.Digests()), Time: time.Now()}
		}
		written = append(written, filepath.ToSlash(rel))
	}
	dels, err := s.state.LoadStagedDeletes()
	if err != nil {
		return written, deleted, err
	}
	for _, rel := range dels {
		abs, err := s.root.ResolveUser(rel)
		if err != nil {
			return written, deleted, fmt.Errorf("staged delete %s: %w", rel, err)
		}
		if err := os.RemoveAll(abs); err != nil {
			return written, deleted, err
		}
		delete(manifest.Files, rel)
		deleted = append(deleted, rel)
	}
	if err := s.state.SaveManifest(manifest); err != nil {
		return written, deleted, err
	}
	if err := s.state.SaveStagedDeletes(nil); err != nil {
		return written, deleted, err
	}
	// Remove now-empty staged directories.
	if err := os.RemoveAll(stagedDir); err != nil {
		return written, deleted, err
	}
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		return written, deleted, err
	}
	return written, deleted, nil
}

// stageDelete records a delete to be applied later. A staged write of the
// same path is discarded.
func (s *Server) stageDelete(rel string) error {
	clean, err := s.root.Clean(rel)
	if err != nil {
		return err
	}
	if _, err := s.root.ResolveUser(clean); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(s.state.StagedDir(), clean))
	dels, err := s.state.LoadStagedDeletes()
	if err != nil {
		return err
	}
	for _, d := range dels {
		if d == filepath.ToSlash(clean) {
			return nil
		}
	}
	return s.state.SaveStagedDeletes(append(dels, filepath.ToSlash(clean)))
}
