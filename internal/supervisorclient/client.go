// Package supervisorclient is the operator's view of one supervisor: a thin
// wrapper over the generated Connect client with the streaming calls
// collapsed into plain Go signatures.
package supervisorclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
)

const writeChunk = 256 * 1024

// Client talks to one supervisor.
type Client struct {
	rpc supervisorv1connect.SupervisorServiceClient
}

type bearer struct{ token string }

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// NewHTTPClient returns an HTTP client speaking cleartext HTTP/2 (h2c) for
// plaintext gRPC inside the pod network.
func NewHTTPClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Protocols: protocols,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     10 * time.Second,
		},
	}}
}

// New creates a client for baseURL (http://host:port) with a bearer token.
func New(httpClient *http.Client, baseURL, token string) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return &Client{rpc: supervisorv1connect.NewSupervisorServiceClient(httpClient, baseURL,
		connect.WithGRPC(), connect.WithInterceptors(bearer{token}))}
}

// Status returns the supervisor's status.
func (c *Client) Status(ctx context.Context) (*supervisorv1.StatusResponse, error) {
	resp, err := c.rpc.Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// SetLaunch persists a launch spec and returns its hash.
func (c *Client) SetLaunch(ctx context.Context, spec *supervisorv1.LaunchSpec) (string, error) {
	resp, err := c.rpc.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: spec}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetLaunchSpecHash(), nil
}

// Start launches the process.
func (c *Client) Start(ctx context.Context) (int32, error) {
	resp, err := c.rpc.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{}))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetPid(), nil
}

// Stop stops the process. Zero timeout uses the launch spec's.
func (c *Client) Stop(ctx context.Context, timeout time.Duration, kill bool) (int32, error) {
	req := &supervisorv1.StopRequest{Kill: kill}
	if timeout > 0 {
		req.Timeout = durationpb.New(timeout)
	}
	resp, err := c.rpc.Stop(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetExitCode(), nil
}

// Restart restarts the process.
func (c *Client) Restart(ctx context.Context) (int32, error) {
	resp, err := c.rpc.Restart(ctx, connect.NewRequest(&supervisorv1.RestartRequest{}))
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetPid(), nil
}

// ListFiles lists a path.
func (c *Client) ListFiles(ctx context.Context, path string, recursive bool, algo supervisorv1.DigestAlgorithm) ([]*supervisorv1.FileInfo, error) {
	resp, err := c.rpc.ListFiles(ctx, connect.NewRequest(&supervisorv1.ListFilesRequest{Path: path, Recursive: recursive, DigestAlgorithm: algo}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetFiles(), nil
}

// Digests returns path → hex digest for regular files under path.
func (c *Client) Digests(ctx context.Context, path string, algo supervisorv1.DigestAlgorithm) (map[string]string, error) {
	files, err := c.ListFiles(ctx, path, true, algo)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		if !f.GetIsDir() && f.GetDigest() != nil {
			out[f.GetPath()] = f.GetDigest().GetHex()
		}
	}
	return out, nil
}

// Download fetches url into path, verifying digest when set.
func (c *Client) Download(ctx context.Context, url, path string, digest *supervisorv1.Digest) (*supervisorv1.DownloadResult, error) {
	stream, err := c.rpc.Download(ctx, connect.NewRequest(&supervisorv1.DownloadRequest{Url: url, Path: path, Expected: digest}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var result *supervisorv1.DownloadResult
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			result = r
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("download ended without a result")
	}
	return result, nil
}

// RunResult is the outcome of a one-shot command.
type RunResult struct {
	ExitCode int32
	Output   []string
}

// Run executes a one-shot command and collects its output.
func (c *Client) Run(ctx context.Context, command string, args []string, workingDir string, env map[string]string, timeout time.Duration) (*RunResult, error) {
	req := &supervisorv1.RunRequest{Command: command, Args: args, WorkingDir: workingDir, Env: env}
	if timeout > 0 {
		req.Timeout = durationpb.New(timeout)
	}
	stream, err := c.rpc.Run(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	res := &RunResult{ExitCode: -1}
	for stream.Receive() {
		if o := stream.Msg().GetOutput(); o != nil {
			res.Output = append(res.Output, o.GetText())
		}
		if r := stream.Msg().GetResult(); r != nil {
			res.ExitCode = r.GetExitCode()
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// ReadFile returns a whole file.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	stream, err := c.rpc.ReadFile(ctx, connect.NewRequest(&supervisorv1.ReadFileRequest{Path: path}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var buf bytes.Buffer
	for stream.Receive() {
		buf.Write(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// IsNotFound reports a missing file or target.
func IsNotFound(err error) bool {
	return connect.CodeOf(err) == connect.CodeNotFound
}

// WriteFile writes data to path, staged or immediate.
func (c *Client) WriteFile(ctx context.Context, path string, data []byte, staged bool) (*supervisorv1.WriteFileResponse, error) {
	mode := supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE
	if staged {
		mode = supervisorv1.WriteMode_WRITE_MODE_STAGED
	}
	stream := c.rpc.WriteFile(ctx)
	if err := stream.Send(&supervisorv1.WriteFileRequest{Message: &supervisorv1.WriteFileRequest_Header{Header: &supervisorv1.WriteFileHeader{Path: path, Mode: mode}}}); err != nil {
		return nil, err
	}
	for off := 0; off < len(data); off += writeChunk {
		end := min(off+writeChunk, len(data))
		if err := stream.Send(&supervisorv1.WriteFileRequest{Message: &supervisorv1.WriteFileRequest_Data{Data: data[off:end]}}); err != nil {
			return nil, err
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// DeleteFile deletes a path, staged or immediate.
func (c *Client) DeleteFile(ctx context.Context, path string, staged, recursive bool) error {
	mode := supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE
	if staged {
		mode = supervisorv1.WriteMode_WRITE_MODE_STAGED
	}
	_, err := c.rpc.DeleteFile(ctx, connect.NewRequest(&supervisorv1.DeleteFileRequest{Path: path, Mode: mode, Recursive: recursive}))
	return err
}

// ApplyStaged applies staged writes and deletes.
func (c *Client) ApplyStaged(ctx context.Context) (*supervisorv1.ApplyStagedResponse, error) {
	resp, err := c.rpc.ApplyStaged(ctx, connect.NewRequest(&supervisorv1.ApplyStagedRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Archive streams a tar of paths into w.
func (c *Client) Archive(ctx context.Context, w io.Writer, paths []string, consistent, gzip bool) error {
	comp := supervisorv1.Compression_COMPRESSION_NONE
	if gzip {
		comp = supervisorv1.Compression_COMPRESSION_GZIP
	}
	stream, err := c.rpc.Archive(ctx, connect.NewRequest(&supervisorv1.ArchiveRequest{Paths: paths, Consistent: consistent, Compression: comp}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		if _, err := w.Write(stream.Msg().GetData()); err != nil {
			return err
		}
	}
	return stream.Err()
}

// DialTunnel opens a tunnel to a named target and returns it as a net.Conn.
func (c *Client) DialTunnel(ctx context.Context, target string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream := c.rpc.Tunnel(ctx)
	if err := stream.Send(&supervisorv1.TunnelRequest{Message: &supervisorv1.TunnelRequest_Open{Open: &supervisorv1.TunnelOpen{Target: target}}}); err != nil {
		cancel()
		return nil, err
	}
	pr, pw := io.Pipe()
	conn := &tunnelConn{stream: stream, reader: pr, cancel: cancel, target: target}
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					_ = pw.Close()
				} else {
					_ = pw.CloseWithError(err)
				}
				return
			}
			if data := msg.GetData(); len(data) > 0 {
				if _, err := pw.Write(data); err != nil {
					return
				}
			}
		}
	}()
	return conn, nil
}

type tunnelConn struct {
	stream *connect.BidiStreamForClient[supervisorv1.TunnelRequest, supervisorv1.TunnelResponse]
	reader *io.PipeReader
	cancel context.CancelFunc
	target string
}

func (t *tunnelConn) Read(p []byte) (int, error) { return t.reader.Read(p) }

func (t *tunnelConn) Write(p []byte) (int, error) {
	if err := t.stream.Send(&supervisorv1.TunnelRequest{Message: &supervisorv1.TunnelRequest_Data{Data: p}}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *tunnelConn) Close() error {
	_ = t.stream.CloseRequest()
	_ = t.stream.CloseResponse()
	t.cancel()
	return t.reader.Close()
}

type tunnelAddr struct{ target string }

func (a tunnelAddr) Network() string { return "supervisor-tunnel" }
func (a tunnelAddr) String() string  { return a.target }

func (t *tunnelConn) LocalAddr() net.Addr              { return tunnelAddr{"local"} }
func (t *tunnelConn) RemoteAddr() net.Addr             { return tunnelAddr{t.target} }
func (t *tunnelConn) SetDeadline(time.Time) error      { return nil }
func (t *tunnelConn) SetReadDeadline(time.Time) error  { return nil }
func (t *tunnelConn) SetWriteDeadline(time.Time) error { return nil }

// String describes the client for logs.
func (c *Client) String() string { return fmt.Sprintf("supervisorclient(%T)", c.rpc) }
