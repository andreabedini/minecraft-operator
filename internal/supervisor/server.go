// Package supervisor implements the in-pod agent: one data directory, one
// supervised process, and a small gRPC API over them.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
)

const (
	readChunk   = 256 * 1024
	tunnelChunk = 32 * 1024
)

// Config configures a Server.
type Config struct {
	Version string
	// DownloadAllowHosts restricts Download to these hosts ("*.example.com"
	// matches subdomains). Empty allows any host.
	DownloadAllowHosts []string
	// AllowInsecureDownloads permits http:// URLs. For tests and local use.
	AllowInsecureDownloads bool
}

// Server implements supervisorv1connect.SupervisorServiceHandler.
type Server struct {
	cfg        Config
	root       Root
	state      *State
	console    *Console
	proc       *Manager
	logger     *slog.Logger
	httpClient *http.Client

	supervisorv1connect.UnimplementedSupervisorServiceHandler
}

// New wires a server over an existing manager.
func New(cfg Config, root Root, state *State, console *Console, proc *Manager, logger *slog.Logger) *Server {
	return &Server{
		cfg:        cfg,
		root:       root,
		state:      state,
		console:    console,
		proc:       proc,
		logger:     logger,
		httpClient: &http.Client{Timeout: 0},
	}
}

// Manager exposes the process manager, for the main package's shutdown path.
func (s *Server) Manager() *Manager { return s.proc }

func (s *Server) requireStopped() error {
	if s.proc.Running() {
		return connect.NewError(connect.CodeFailedPrecondition, ErrRunning)
	}
	return nil
}

func pathError(err error) error {
	switch {
	case errors.Is(err, ErrOutsideRoot), errors.Is(err, ErrReserved):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, os.ErrNotExist):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, os.ErrPermission):
		return connect.NewError(connect.CodePermissionDenied, err)
	default:
		return err
	}
}

// Status implements the RPC.
func (s *Server) Status(_ context.Context, _ *connect.Request[supervisorv1.StatusRequest]) (*connect.Response[supervisorv1.StatusResponse], error) {
	snap := s.proc.Status()
	resp := &supervisorv1.StatusResponse{
		State:             snap.State,
		Pid:               int32(snap.PID),
		HasLaunchSpec:     snap.Spec != nil,
		LaunchSpecHash:    snap.SpecHash,
		StagedCount:       int32(s.stagedCount()),
		SupervisorVersion: s.cfg.Version,
		DataRoot:          s.root.Dir,
	}
	if !snap.StartedAt.IsZero() && snap.State != supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		resp.StartedAt = timestamppb.New(snap.StartedAt)
	}
	if snap.LastExit != nil {
		code := int32(snap.LastExit.Code)
		resp.LastExitCode = &code
		resp.LastExitAt = timestamppb.New(snap.LastExit.At)
	}
	return connect.NewResponse(resp), nil
}

// SetLaunch implements the RPC.
func (s *Server) SetLaunch(_ context.Context, req *connect.Request[supervisorv1.SetLaunchRequest]) (*connect.Response[supervisorv1.SetLaunchResponse], error) {
	hash, err := s.proc.SetSpec(req.Msg.GetSpec())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&supervisorv1.SetLaunchResponse{LaunchSpecHash: hash}), nil
}

// Start implements the RPC.
func (s *Server) Start(ctx context.Context, _ *connect.Request[supervisorv1.StartRequest]) (*connect.Response[supervisorv1.StartResponse], error) {
	pid, err := s.proc.Start(ctx)
	switch {
	case errors.Is(err, ErrRunning):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrNoLaunchSpec):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, err
	}
	return connect.NewResponse(&supervisorv1.StartResponse{Pid: int32(pid)}), nil
}

// Stop implements the RPC.
func (s *Server) Stop(ctx context.Context, req *connect.Request[supervisorv1.StopRequest]) (*connect.Response[supervisorv1.StopResponse], error) {
	code, err := s.proc.Stop(ctx, req.Msg.GetTimeout().AsDuration(), req.Msg.GetKill())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&supervisorv1.StopResponse{ExitCode: int32(code)}), nil
}

// Restart implements the RPC.
func (s *Server) Restart(ctx context.Context, req *connect.Request[supervisorv1.RestartRequest]) (*connect.Response[supervisorv1.RestartResponse], error) {
	pid, err := s.proc.Restart(ctx, req.Msg.GetStopTimeout().AsDuration())
	if errors.Is(err, ErrNoLaunchSpec) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&supervisorv1.RestartResponse{Pid: int32(pid)}), nil
}

// Console implements the RPC.
func (s *Server) Console(ctx context.Context, stream *connect.BidiStream[supervisorv1.ConsoleRequest, supervisorv1.ConsoleResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	sub := first.GetSubscribe()
	if sub == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first message must be subscribe"))
	}
	lines, cancel := s.console.Subscribe(int(sub.GetReplayLines()))
	defer cancel()

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	inputErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					inputErr <- nil
				} else {
					inputErr <- err
				}
				return
			}
			if in, ok := msg.GetMessage().(*supervisorv1.ConsoleRequest_Input); ok {
				if err := s.proc.WriteInput(in.Input); err != nil {
					s.console.Append(supervisorv1.Stream_STREAM_SUPERVISOR, "input dropped: "+err.Error())
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-inputErr:
			return err
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := stream.Send(&supervisorv1.ConsoleResponse{Line: &supervisorv1.ConsoleLine{
				Sequence: line.Seq,
				Time:     timestamppb.New(line.Time),
				Stream:   line.Stream,
				Text:     line.Text,
			}}); err != nil {
				return err
			}
		}
	}
}

// Download implements the RPC.
func (s *Server) Download(ctx context.Context, req *connect.Request[supervisorv1.DownloadRequest], stream *connect.ServerStream[supervisorv1.DownloadResponse]) error {
	if err := s.requireStopped(); err != nil {
		return err
	}
	result, err := s.download(ctx, req.Msg, stream.Send)
	if err != nil {
		if errors.Is(err, ErrOutsideRoot) || errors.Is(err, ErrReserved) || strings.HasPrefix(err.Error(), "url:") {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		return err
	}
	return stream.Send(&supervisorv1.DownloadResponse{Event: &supervisorv1.DownloadResponse_Result{Result: result}})
}

// Run implements the RPC.
func (s *Server) Run(ctx context.Context, req *connect.Request[supervisorv1.RunRequest], stream *connect.ServerStream[supervisorv1.RunResponse]) error {
	if err := s.requireStopped(); err != nil {
		return err
	}
	msg := req.Msg
	if strings.TrimSpace(msg.GetCommand()) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("command is required"))
	}
	dir, err := s.root.Resolve(msg.GetWorkingDir())
	if err != nil {
		return pathError(err)
	}
	if timeout := msg.GetTimeout().AsDuration(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, msg.GetCommand(), msg.GetArgs()...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(msg.GetEnv())
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.logger.Info("run started", "pid", cmd.Process.Pid, "command", msg.GetCommand(), "args", msg.GetArgs())

	type out struct {
		stream supervisorv1.Stream
		text   string
	}
	lines := make(chan out, 256)
	pump := func(r io.Reader, st supervisorv1.Stream, done chan<- struct{}) {
		defer close(done)
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				lines <- out{st, strings.TrimRight(line, "\r\n")}
			}
			if err != nil {
				return
			}
		}
	}
	outDone, errDone := make(chan struct{}), make(chan struct{})
	go pump(stdout, supervisorv1.Stream_STREAM_STDOUT, outDone)
	go pump(stderr, supervisorv1.Stream_STREAM_STDERR, errDone)
	go func() {
		<-outDone
		<-errDone
		close(lines)
	}()
	for l := range lines {
		if err := stream.Send(&supervisorv1.RunResponse{Event: &supervisorv1.RunResponse_Output{Output: &supervisorv1.OutputLine{Stream: l.stream, Text: l.text}}}); err != nil {
			_ = cmd.Process.Kill()
			<-outDone
			<-errDone
			_ = cmd.Wait()
			return err
		}
	}
	waitErr := cmd.Wait()
	code := exitCode(cmd, waitErr)
	s.logger.Info("run finished", "code", code, "duration", time.Since(started))
	return stream.Send(&supervisorv1.RunResponse{Event: &supervisorv1.RunResponse_Result{Result: &supervisorv1.RunResult{
		ExitCode: int32(code),
		Duration: durationpb.New(time.Since(started)),
	}}})
}

// ListFiles implements the RPC.
func (s *Server) ListFiles(_ context.Context, req *connect.Request[supervisorv1.ListFilesRequest]) (*connect.Response[supervisorv1.ListFilesResponse], error) {
	files, err := s.listFiles(req.Msg.GetPath(), req.Msg.GetRecursive(), req.Msg.GetDigestAlgorithm())
	if err != nil {
		return nil, pathError(err)
	}
	return connect.NewResponse(&supervisorv1.ListFilesResponse{Files: files}), nil
}

// ReadFile implements the RPC.
func (s *Server) ReadFile(ctx context.Context, req *connect.Request[supervisorv1.ReadFileRequest], stream *connect.ServerStream[supervisorv1.ReadFileResponse]) error {
	abs, err := s.root.ResolveUser(req.Msg.GetPath())
	if err != nil {
		return pathError(err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return pathError(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("path is a directory"))
	}
	rel, _ := s.root.Rel(abs)
	if err := stream.Send(&supervisorv1.ReadFileResponse{Message: &supervisorv1.ReadFileResponse_Info{Info: fileInfoOf(rel, fi)}}); err != nil {
		return err
	}
	var r io.Reader = f
	if off := req.Msg.GetOffset(); off > 0 {
		if _, err := f.Seek(int64(off), io.SeekStart); err != nil {
			return err
		}
	}
	if n := req.Msg.GetLength(); n > 0 {
		r = io.LimitReader(r, int64(n))
	}
	buf := make([]byte, readChunk)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&supervisorv1.ReadFileResponse{Message: &supervisorv1.ReadFileResponse_Data{Data: buf[:n]}}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// WriteFile implements the RPC.
func (s *Server) WriteFile(_ context.Context, stream *connect.ClientStream[supervisorv1.WriteFileRequest]) (*connect.Response[supervisorv1.WriteFileResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("empty stream"))
	}
	hdr := stream.Msg().GetHeader()
	if hdr == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must be the header"))
	}
	staged := hdr.GetMode() != supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE
	if !staged {
		if err := s.requireStopped(); err != nil {
			return nil, err
		}
	}
	dest, err := s.destination(hdr.GetPath(), staged)
	if err != nil {
		return nil, pathError(err)
	}
	result, err := receiveToFile(dest, fileMode(hdr.GetFileMode(), 0o644), func() ([]byte, error) {
		if !stream.Receive() {
			if err := stream.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		data, ok := stream.Msg().GetMessage().(*supervisorv1.WriteFileRequest_Data)
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("expected data after the header"))
		}
		return data.Data, nil
	})
	if err != nil {
		return nil, err
	}
	if !staged {
		rel, _ := s.root.Rel(dest)
		if err := s.state.RecordFile(rel, ManifestEntry{Size: result.Size, Digests: digestsToMap(result.Digests), Time: time.Now()}); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&supervisorv1.WriteFileResponse{
		Size:    uint64(max(result.Size, 0)),
		Digests: result.Digests,
		Staged:  staged,
	}), nil
}

// DeleteFile implements the RPC.
func (s *Server) DeleteFile(_ context.Context, req *connect.Request[supervisorv1.DeleteFileRequest]) (*connect.Response[supervisorv1.DeleteFileResponse], error) {
	msg := req.Msg
	if msg.GetMode() != supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE {
		if err := s.stageDelete(msg.GetPath()); err != nil {
			return nil, pathError(err)
		}
		return connect.NewResponse(&supervisorv1.DeleteFileResponse{}), nil
	}
	if err := s.requireStopped(); err != nil {
		return nil, err
	}
	abs, err := s.root.ResolveUser(msg.GetPath())
	if err != nil {
		return nil, pathError(err)
	}
	if abs == s.root.Dir {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("refusing to delete the data root"))
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, pathError(err)
	}
	if fi.IsDir() && !msg.GetRecursive() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("path is a directory; set recursive"))
	}
	if err := os.RemoveAll(abs); err != nil {
		return nil, pathError(err)
	}
	rel, _ := s.root.Rel(abs)
	_ = s.state.ForgetFile(rel)
	return connect.NewResponse(&supervisorv1.DeleteFileResponse{}), nil
}

// ApplyStaged implements the RPC.
func (s *Server) ApplyStaged(_ context.Context, _ *connect.Request[supervisorv1.ApplyStagedRequest]) (*connect.Response[supervisorv1.ApplyStagedResponse], error) {
	if err := s.requireStopped(); err != nil {
		return nil, err
	}
	written, deleted, err := s.applyStaged()
	if err != nil {
		return nil, err
	}
	s.logger.Info("applied staged changes", "written", len(written), "deleted", len(deleted))
	return connect.NewResponse(&supervisorv1.ApplyStagedResponse{Written: written, Deleted: deleted}), nil
}

// Archive implements the RPC.
func (s *Server) Archive(ctx context.Context, req *connect.Request[supervisorv1.ArchiveRequest], stream *connect.ServerStream[supervisorv1.ArchiveResponse]) error {
	err := s.archive(ctx, req.Msg, func(chunk []byte) error {
		return stream.Send(&supervisorv1.ArchiveResponse{Data: chunk})
	})
	if err != nil {
		return pathError(err)
	}
	return nil
}

// Tunnel implements the RPC.
func (s *Server) Tunnel(ctx context.Context, stream *connect.BidiStream[supervisorv1.TunnelRequest, supervisorv1.TunnelResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first message must be open"))
	}
	target, ok := s.proc.Spec().GetTunnelTargets()[open.GetTarget()]
	if !ok {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown tunnel target %q", open.GetTarget()))
	}
	if err := validateTunnelTarget(target.GetAddress()); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target.GetAddress())
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	defer conn.Close()

	errs := make(chan error, 2)
	go func() {
		buf := make([]byte, tunnelChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if err := stream.Send(&supervisorv1.TunnelResponse{Data: buf[:n]}); err != nil {
					errs <- err
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					errs <- nil
				} else {
					errs <- err
				}
				return
			}
		}
	}()
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errs <- nil
				} else {
					errs <- err
				}
				return
			}
			if data := msg.GetData(); len(data) > 0 {
				if _, err := conn.Write(data); err != nil {
					errs <- err
					return
				}
			}
		}
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return nil
	}
}
