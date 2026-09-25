package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

const (
	launchFileName        = "launch.json"
	manifestFileName      = "manifest.json"
	stagedDirName         = "staged"
	stagedDeletesFileName = "staged-deletes.json"
)

// State persists the supervisor's own records under <root>/.supervisor.
type State struct {
	dir string
}

// ManifestEntry records a file the supervisor put in place.
type ManifestEntry struct {
	Size    int64             `json:"size"`
	Digests map[string]string `json:"digests"`
	Source  string            `json:"source,omitempty"`
	Time    time.Time         `json:"time"`
}

// Manifest is the set of files installed through the supervisor, keyed by
// API path.
type Manifest struct {
	Files map[string]ManifestEntry `json:"files"`
}

// NewState creates the state directory if needed.
func NewState(root Root) (*State, error) {
	s := &State{dir: root.StateDir()}
	if err := os.MkdirAll(filepath.Join(s.dir, stagedDirName), 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

// StagedDir is where staged writes live until ApplyStaged.
func (s *State) StagedDir() string {
	return filepath.Join(s.dir, stagedDirName)
}

// LoadLaunch returns the persisted launch spec, or nil if none exists.
func (s *State) LoadLaunch() (*supervisorv1.LaunchSpec, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, launchFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	spec := &supervisorv1.LaunchSpec{}
	if err := protojson.Unmarshal(data, spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", launchFileName, err)
	}
	return spec, nil
}

// SaveLaunch persists the launch spec atomically.
func (s *State) SaveLaunch(spec *supervisorv1.LaunchSpec) error {
	data, err := protojson.MarshalOptions{Multiline: true}.Marshal(spec)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, launchFileName), data, 0o600)
}

// LaunchHash is a stable digest of a launch spec.
func LaunchHash(spec *supervisorv1.LaunchSpec) string {
	if spec == nil {
		return ""
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// LoadManifest returns the manifest, empty if none exists.
func (s *State) LoadManifest() (*Manifest, error) {
	m := &Manifest{Files: map[string]ManifestEntry{}}
	data, err := os.ReadFile(filepath.Join(s.dir, manifestFileName))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestFileName, err)
	}
	if m.Files == nil {
		m.Files = map[string]ManifestEntry{}
	}
	return m, nil
}

// SaveManifest persists the manifest atomically.
func (s *State) SaveManifest(m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, manifestFileName), data, 0o644)
}

// RecordFile updates one manifest entry.
func (s *State) RecordFile(path string, entry ManifestEntry) error {
	m, err := s.LoadManifest()
	if err != nil {
		return err
	}
	m.Files[path] = entry
	return s.SaveManifest(m)
}

// ForgetFile removes one manifest entry, if present.
func (s *State) ForgetFile(path string) error {
	m, err := s.LoadManifest()
	if err != nil {
		return err
	}
	if _, ok := m.Files[path]; !ok {
		return nil
	}
	delete(m.Files, path)
	return s.SaveManifest(m)
}

// LoadStagedDeletes returns the API paths scheduled for deletion on apply.
func (s *State) LoadStagedDeletes() ([]string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, stagedDeletesFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("parse %s: %w", stagedDeletesFileName, err)
	}
	return paths, nil
}

// SaveStagedDeletes persists the staged deletion list. An empty list removes
// the file.
func (s *State) SaveStagedDeletes(paths []string) error {
	file := filepath.Join(s.dir, stagedDeletesFileName)
	if len(paths) == 0 {
		err := os.Remove(file)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(file, data, 0o644)
}

// writeFileAtomic writes data to a temporary file next to path and renames it
// into place.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
