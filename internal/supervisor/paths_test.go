package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestRoot(t *testing.T) Root {
	t.Helper()
	dir := t.TempDir()
	root, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRootResolveStaysInside(t *testing.T) {
	root := newTestRoot(t)
	cases := []struct {
		rel     string
		wantRel string
		wantErr error
	}{
		{"", ".", nil},
		{".", ".", nil},
		{"a/b", "a/b", nil},
		{"./a/../b", "b", nil},
		{"../etc/passwd", "etc/passwd", nil}, // leading .. is swallowed, not escaped
		{"/etc/passwd", "", ErrOutsideRoot},
		{".supervisor/launch.json", "", ErrReserved},
		{".supervisor", "", ErrReserved},
	}
	for _, c := range cases {
		got, err := root.ResolveUser(c.rel)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("ResolveUser(%q) err = %v, want %v", c.rel, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveUser(%q) unexpected error: %v", c.rel, err)
			continue
		}
		want := root.Dir
		if c.wantRel != "." {
			want = filepath.Join(root.Dir, c.wantRel)
		}
		if got != want {
			t.Errorf("ResolveUser(%q) = %q, want %q", c.rel, got, want)
		}
	}
}

func TestRootRejectsSymlinkEscape(t *testing.T) {
	root := newTestRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root.Dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ResolveUser("escape/file"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("symlink escape not rejected: %v", err)
	}
	if _, err := root.ResolveUser("escape"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("symlink itself not rejected: %v", err)
	}
	// A symlink that stays inside is fine.
	if err := os.MkdirAll(filepath.Join(root.Dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root.Dir, "alias")); err != nil {
		t.Fatal(err)
	}
	got, err := root.ResolveUser("alias/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root.Dir, "real", "x"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRootRel(t *testing.T) {
	root := newTestRoot(t)
	rel, err := root.Rel(filepath.Join(root.Dir, "a", "b"))
	if err != nil || rel != "a/b" {
		t.Errorf("Rel = %q, %v", rel, err)
	}
	if _, err := root.Rel(filepath.Dir(root.Dir)); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Rel outside = %v", err)
	}
}
