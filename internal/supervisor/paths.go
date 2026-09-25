package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// stateDirName is the directory under the data root that holds the
// supervisor's own state. It is never exposed through the file API.
const stateDirName = ".supervisor"

var (
	// ErrOutsideRoot is returned for paths that escape the data root, either
	// lexically or through a symlink.
	ErrOutsideRoot = errors.New("path escapes the data root")
	// ErrReserved is returned for paths under the supervisor's state directory.
	ErrReserved = errors.New("path is reserved for supervisor state")
)

// Root is the data directory the supervisor manages. All API paths are
// relative to it.
type Root struct {
	// Dir is the absolute, symlink-resolved path of the data root.
	Dir string
}

// NewRoot resolves dir and checks it is a directory.
func NewRoot(dir string) (Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Root{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return Root{}, err
	}
	if !fi.IsDir() {
		return Root{}, errors.New("data root is not a directory")
	}
	return Root{Dir: real}, nil
}

// Clean normalises a relative API path. Absolute paths and paths that climb
// above the root are rejected; "." and "" both mean the root itself.
func (r Root) Clean(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", ErrOutsideRoot
	}
	// Prefixing "/" makes Clean swallow any leading "..".
	clean := filepath.Clean("/" + rel)
	if clean == "/" {
		return ".", nil
	}
	return strings.TrimPrefix(clean, "/"), nil
}

// Resolve returns the absolute path for rel, guaranteeing that every existing
// component, followed through symlinks, stays inside the root. The returned
// path has symlinks in existing components resolved.
func (r Root) Resolve(rel string) (string, error) {
	clean, err := r.Clean(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(r.Dir, clean)
	real, err := evalExistingPrefix(abs)
	if err != nil {
		return "", err
	}
	if !r.contains(real) {
		return "", ErrOutsideRoot
	}
	return real, nil
}

// ResolveUser is Resolve for paths supplied through the public API: it also
// rejects the supervisor's state directory.
func (r Root) ResolveUser(rel string) (string, error) {
	clean, err := r.Clean(rel)
	if err != nil {
		return "", err
	}
	if clean == stateDirName || strings.HasPrefix(clean, stateDirName+string(os.PathSeparator)) {
		return "", ErrReserved
	}
	return r.Resolve(clean)
}

// Rel converts an absolute path inside the root back to an API path.
func (r Root) Rel(abs string) (string, error) {
	rel, err := filepath.Rel(r.Dir, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return ".", nil
	}
	if strings.HasPrefix(rel, "..") {
		return "", ErrOutsideRoot
	}
	return filepath.ToSlash(rel), nil
}

// StateDir is the absolute path of the supervisor's state directory.
func (r Root) StateDir() string {
	return filepath.Join(r.Dir, stateDirName)
}

func (r Root) contains(abs string) bool {
	return abs == r.Dir || strings.HasPrefix(abs, r.Dir+string(os.PathSeparator))
}

// evalExistingPrefix resolves symlinks in the longest existing prefix of abs
// and re-attaches the non-existing remainder.
func evalExistingPrefix(abs string) (string, error) {
	var rest []string
	cur := abs
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			parts := append([]string{real}, rest...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}
