package supervisor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
)

const archiveChunk = 64 * 1024

// chunkWriter turns a byte stream into fixed-size sends.
type chunkWriter struct {
	send func([]byte) error
	buf  []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		space := archiveChunk - len(w.buf)
		n := min(space, len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		if len(w.buf) == archiveChunk {
			if err := w.Flush(); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (w *chunkWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	chunk := make([]byte, len(w.buf))
	copy(chunk, w.buf)
	w.buf = w.buf[:0]
	return w.send(chunk)
}

// archive writes a tar of the requested paths. With consistent it runs the
// launch spec's quiesce hook first and the resume hook afterwards.
func (s *Server) archive(ctx context.Context, req *supervisorv1.ArchiveRequest, send func([]byte) error) error {
	paths := req.GetPaths()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	roots := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := s.root.ResolveUser(p)
		if err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
		roots = append(roots, abs)
	}

	if req.GetConsistent() {
		spec := s.proc.Spec()
		if err := s.proc.RunHook(ctx, spec.GetQuiesce()); err != nil {
			return fmt.Errorf("quiesce: %w", err)
		}
		defer func() {
			// Resume even if the client went away.
			if err := s.proc.RunHook(context.WithoutCancel(ctx), spec.GetResume()); err != nil {
				s.logger.Error("resume hook failed", "error", err)
			}
		}()
	}

	cw := &chunkWriter{send: send}
	var w io.Writer = cw
	var gz *gzip.Writer
	if req.GetCompression() == supervisorv1.Compression_COMPRESSION_GZIP {
		gz = gzip.NewWriter(cw)
		w = gz
	}
	tw := tar.NewWriter(w)
	for _, root := range roots {
		if err := s.tarTree(ctx, tw, root); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return err
		}
	}
	return cw.Flush()
}

func (s *Server) tarTree(ctx context.Context, tw *tar.Writer, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && path == s.root.StateDir() {
			return filepath.SkipDir
		}
		rel, err := s.root.Rel(path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		// Copy at most the header size: the file may grow while we read.
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil && err != io.EOF {
			return err
		}
		return nil
	})
}
