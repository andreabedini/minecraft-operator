package supervisor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/durationpb"

	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
)

const (
	testFullToken     = "full-token"
	testReadOnlyToken = "ro-token"
)

type testEnv struct {
	root   Root
	server *Server
	url    string
	http   *http.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := newTestRoot(t)
	state, err := NewState(root)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	console := NewConsole(100)
	mgr, err := NewManager(root, state, console, logger)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Passthrough = nil
	srv := New(Config{Version: "test", AllowInsecureDownloads: true}, root, state, console, mgr, logger)
	mux := http.NewServeMux()
	mux.Handle(supervisorv1connect.NewSupervisorServiceHandler(srv,
		connect.WithInterceptors(NewAuthInterceptor(Tokens{Full: testFullToken, ReadOnly: testReadOnlyToken}))))
	hs := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	hs.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = mgr.Stop(ctx, time.Second, true)
		hs.Close()
	})
	// Bidi streams need HTTP/2; speak h2c to the plaintext test server.
	h2 := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	return &testEnv{root: root, server: srv, url: hs.URL, http: h2}
}

func (e *testEnv) client(token string) supervisorv1connect.SupervisorServiceClient {
	return supervisorv1connect.NewSupervisorServiceClient(e.http, e.url, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}

// streamBearer adds the header to streaming calls too.
type streamBearer struct{ token string }

func (b streamBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b streamBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b streamBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (e *testEnv) fullClient() supervisorv1connect.SupervisorServiceClient {
	return supervisorv1connect.NewSupervisorServiceClient(e.http, e.url, connect.WithGRPC(),
		connect.WithInterceptors(streamBearer{testFullToken}))
}

func (e *testEnv) roClient() supervisorv1connect.SupervisorServiceClient {
	return supervisorv1connect.NewSupervisorServiceClient(e.http, e.url, connect.WithGRPC(),
		connect.WithInterceptors(streamBearer{testReadOnlyToken}))
}

// echoSpec runs a shell loop that echoes stdin lines and exits on "stop".
func echoSpec() *supervisorv1.LaunchSpec {
	return &supervisorv1.LaunchSpec{
		Command: "sh",
		Args: []string{"-c", `echo booting; echo "Done (1.2s)!"; while read -r line; do
  if [ "$line" = "stop" ]; then echo "Stopping server"; exit 0; fi
  if [ "$line" = "save-all flush" ]; then echo "Saved the game"; continue; fi
  echo "got: $line"
done`},
		StopCommand: "stop",
		StopTimeout: durationpb.New(5 * time.Second),
		Quiesce:     &supervisorv1.ConsoleHook{Commands: []string{"save-off", "save-all flush"}, WaitFor: "Saved the game", Timeout: durationpb.New(5 * time.Second)},
		Resume:      &supervisorv1.ConsoleHook{Commands: []string{"save-on"}},
	}
}

func TestAuth(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, err := env.client("").Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: code %v, want unauthenticated", connect.CodeOf(err))
	}
	_, err = env.client("wrong").Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("wrong token: code %v", connect.CodeOf(err))
	}
	if _, err := env.roClient().Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{})); err != nil {
		t.Errorf("read-only Status: %v", err)
	}
	_, err = env.roClient().SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: echoSpec()}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("read-only SetLaunch: code %v, want permission denied", connect.CodeOf(err))
	}
	// Streaming handler path.
	stream, err := env.roClient().ReadFile(ctx, connect.NewRequest(&supervisorv1.ReadFileRequest{Path: "missing"}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if connect.CodeOf(stream.Err()) != connect.CodeNotFound {
		t.Errorf("read-only ReadFile missing: %v", stream.Err())
	}
	dl, err := env.roClient().Download(ctx, connect.NewRequest(&supervisorv1.DownloadRequest{Url: "https://example.invalid/x", Path: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for dl.Receive() {
	}
	if connect.CodeOf(dl.Err()) != connect.CodePermissionDenied {
		t.Errorf("read-only Download: %v", dl.Err())
	}
}

func TestProcessLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()

	st, err := c.Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Msg.GetHasLaunchSpec() || st.Msg.GetState() != supervisorv1.ProcessState_PROCESS_STATE_STOPPED {
		t.Errorf("initial status = %v", st.Msg)
	}
	_, err = c.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("Start without spec: %v", err)
	}

	sl, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: echoSpec()}))
	if err != nil {
		t.Fatal(err)
	}
	if sl.Msg.GetLaunchSpecHash() == "" {
		t.Error("empty launch spec hash")
	}
	if _, err := os.Stat(filepath.Join(env.root.StateDir(), launchFileName)); err != nil {
		t.Errorf("launch spec not persisted: %v", err)
	}

	start, err := c.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if start.Msg.GetPid() <= 0 {
		t.Errorf("pid = %d", start.Msg.GetPid())
	}
	_, err = c.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second Start: %v", err)
	}

	// Install operations are refused while running.
	_, err = c.ApplyStaged(ctx, connect.NewRequest(&supervisorv1.ApplyStagedRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("ApplyStaged while running: %v", err)
	}

	// Console: replay, then a command and its echo.
	console := c.Console(ctx)
	if err := console.Send(&supervisorv1.ConsoleRequest{Message: &supervisorv1.ConsoleRequest_Subscribe{Subscribe: &supervisorv1.ConsoleSubscribe{ReplayLines: 50}}}); err != nil {
		t.Fatal(err)
	}
	waitLine := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := console.Receive()
			if err != nil {
				t.Fatalf("console receive: %v", err)
			}
			if strings.Contains(resp.GetLine().GetText(), substr) {
				return
			}
		}
		t.Fatalf("did not see %q on the console", substr)
	}
	waitLine("Done (1.2s)!")
	if err := console.Send(&supervisorv1.ConsoleRequest{Message: &supervisorv1.ConsoleRequest_Input{Input: "hello"}}); err != nil {
		t.Fatal(err)
	}
	waitLine("got: hello")

	// Consistent archive drives the quiesce and resume hooks.
	arch, err := c.Archive(ctx, connect.NewRequest(&supervisorv1.ArchiveRequest{Consistent: true}))
	if err != nil {
		t.Fatal(err)
	}
	var tarBuf bytes.Buffer
	for arch.Receive() {
		tarBuf.Write(arch.Msg().GetData())
	}
	if arch.Err() != nil {
		t.Fatalf("archive: %v", arch.Err())
	}
	waitLine("got: save-on")

	// Restart keeps working and changes the pid.
	rs, err := c.Restart(ctx, connect.NewRequest(&supervisorv1.RestartRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if rs.Msg.GetPid() == start.Msg.GetPid() {
		t.Error("restart returned the same pid")
	}
	_ = console.CloseRequest()
	_ = console.CloseResponse()

	stop, err := c.Stop(ctx, connect.NewRequest(&supervisorv1.StopRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if stop.Msg.GetExitCode() != 0 {
		t.Errorf("exit code = %d", stop.Msg.GetExitCode())
	}
	st, err = c.Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Msg.GetState() != supervisorv1.ProcessState_PROCESS_STATE_STOPPED || st.Msg.LastExitCode == nil || *st.Msg.LastExitCode != 0 {
		t.Errorf("status after stop = %v", st.Msg)
	}
}

func TestStopTimeoutKills(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()
	spec := &supervisorv1.LaunchSpec{
		Command:     "sh",
		Args:        []string{"-c", "trap '' TERM; while true; do sleep 1; done"},
		StopCommand: "stop", // ignored by the script
		StopTimeout: durationpb.New(300 * time.Millisecond),
	}
	if _, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: spec})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{})); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	stop, err := c.Stop(ctx, connect.NewRequest(&supervisorv1.StopRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 5*time.Second {
		t.Errorf("stop took %s", time.Since(began))
	}
	if stop.Msg.GetExitCode() != 128+9 {
		t.Errorf("exit code = %d, want 137 (killed)", stop.Msg.GetExitCode())
	}
}

func TestRestartPolicyOnFailure(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()
	spec := &supervisorv1.LaunchSpec{
		Command:       "sh",
		Args:          []string{"-c", "echo crash; exit 3"},
		RestartPolicy: supervisorv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
	}
	if _, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: spec})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(ctx, connect.NewRequest(&supervisorv1.StartRequest{})); err != nil {
		t.Fatal(err)
	}
	// minBackoff is 1s; after ~1.5s a second run must have happened.
	time.Sleep(1500 * time.Millisecond)
	crashes := 0
	for _, l := range env.server.console.Recent(100) {
		if l.Text == "crash" {
			crashes++
		}
	}
	if crashes < 2 {
		t.Errorf("saw %d crash lines, want at least 2 (automatic restart)", crashes)
	}
	// Stop cancels any pending restart.
	if _, err := c.Stop(ctx, connect.NewRequest(&supervisorv1.StopRequest{})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if env.server.proc.Running() {
		t.Error("process restarted after Stop")
	}
}

func TestFilesStagedAndImmediate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()

	write := func(path string, mode supervisorv1.WriteMode, content string) *supervisorv1.WriteFileResponse {
		t.Helper()
		ws := c.WriteFile(ctx)
		if err := ws.Send(&supervisorv1.WriteFileRequest{Message: &supervisorv1.WriteFileRequest_Header{Header: &supervisorv1.WriteFileHeader{Path: path, Mode: mode}}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(content); i += 3 {
			end := min(i+3, len(content))
			if err := ws.Send(&supervisorv1.WriteFileRequest{Message: &supervisorv1.WriteFileRequest_Data{Data: []byte(content[i:end])}}); err != nil {
				t.Fatal(err)
			}
		}
		resp, err := ws.CloseAndReceive()
		if err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return resp.Msg
	}

	imm := write("server.properties", supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE, "server-port=25565\n")
	if imm.GetStaged() || imm.GetSize() != 18 {
		t.Errorf("immediate write = %v", imm)
	}
	want := sha256.Sum256([]byte("server-port=25565\n"))
	found := false
	for _, d := range imm.GetDigests() {
		if d.GetAlgorithm() == supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 && d.GetHex() == hex.EncodeToString(want[:]) {
			found = true
		}
	}
	if !found {
		t.Errorf("sha256 missing or wrong: %v", imm.GetDigests())
	}

	st := write("config/mod.toml", supervisorv1.WriteMode_WRITE_MODE_STAGED, "a = 1\n")
	if !st.GetStaged() {
		t.Error("staged write not reported as staged")
	}
	if _, err := os.Stat(filepath.Join(env.root.Dir, "config", "mod.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Error("staged file appeared in place before apply")
	}
	if _, err := c.DeleteFile(ctx, connect.NewRequest(&supervisorv1.DeleteFileRequest{Path: "server.properties", Mode: supervisorv1.WriteMode_WRITE_MODE_STAGED})); err != nil {
		t.Fatal(err)
	}
	status, err := c.Status(ctx, connect.NewRequest(&supervisorv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if status.Msg.GetStagedCount() != 2 {
		t.Errorf("staged count = %d, want 2", status.Msg.GetStagedCount())
	}

	applied, err := c.ApplyStaged(ctx, connect.NewRequest(&supervisorv1.ApplyStagedRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Msg.GetWritten()) != 1 || applied.Msg.GetWritten()[0] != "config/mod.toml" {
		t.Errorf("written = %v", applied.Msg.GetWritten())
	}
	if len(applied.Msg.GetDeleted()) != 1 || applied.Msg.GetDeleted()[0] != "server.properties" {
		t.Errorf("deleted = %v", applied.Msg.GetDeleted())
	}
	if data, err := os.ReadFile(filepath.Join(env.root.Dir, "config", "mod.toml")); err != nil || string(data) != "a = 1\n" {
		t.Errorf("applied content = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(env.root.Dir, "server.properties")); !errors.Is(err, os.ErrNotExist) {
		t.Error("staged delete not applied")
	}

	// Reserved and escaping paths.
	_, err = c.ListFiles(ctx, connect.NewRequest(&supervisorv1.ListFilesRequest{Path: ".supervisor"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("list .supervisor: %v", err)
	}
	_, err = c.ListFiles(ctx, connect.NewRequest(&supervisorv1.ListFilesRequest{Path: "/etc"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("list /etc: %v", err)
	}

	// Listing hides the state directory, recursive includes nested files.
	ls, err := c.ListFiles(ctx, connect.NewRequest(&supervisorv1.ListFilesRequest{Path: ".", Recursive: true, DigestAlgorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1}))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range ls.Msg.GetFiles() {
		paths = append(paths, f.GetPath())
		if strings.HasPrefix(f.GetPath(), ".supervisor") {
			t.Errorf("state dir leaked: %s", f.GetPath())
		}
		if f.GetPath() == "config/mod.toml" && f.GetDigest().GetHex() == "" {
			t.Error("digest not computed")
		}
	}
	if strings.Join(paths, ",") != "config,config/mod.toml" {
		t.Errorf("paths = %v", paths)
	}

	// ReadFile with offset and length.
	rf, err := c.ReadFile(ctx, connect.NewRequest(&supervisorv1.ReadFileRequest{Path: "config/mod.toml", Offset: 2, Length: 3}))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	var info *supervisorv1.FileInfo
	for rf.Receive() {
		if i := rf.Msg().GetInfo(); i != nil {
			info = i
		}
		got.Write(rf.Msg().GetData())
	}
	if rf.Err() != nil {
		t.Fatal(rf.Err())
	}
	if info == nil || info.GetSize() != 6 {
		t.Errorf("info = %v", info)
	}
	if got.String() != "= 1" {
		t.Errorf("read = %q", got.String())
	}

	// Immediate delete of a directory needs recursive.
	_, err = c.DeleteFile(ctx, connect.NewRequest(&supervisorv1.DeleteFileRequest{Path: "config", Mode: supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete dir without recursive: %v", err)
	}
	if _, err := c.DeleteFile(ctx, connect.NewRequest(&supervisorv1.DeleteFileRequest{Path: "config", Mode: supervisorv1.WriteMode_WRITE_MODE_IMMEDIATE, Recursive: true})); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadVerifiesDigest(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()
	payload := bytes.Repeat([]byte("minecraft"), 100000) // ~900 KB
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer fs.Close()
	sum := sha256.Sum256(payload)

	run := func(req *supervisorv1.DownloadRequest) (*supervisorv1.DownloadResult, error) {
		stream, err := c.Download(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		var result *supervisorv1.DownloadResult
		progress := 0
		for stream.Receive() {
			if p := stream.Msg().GetProgress(); p != nil {
				progress++
			}
			if r := stream.Msg().GetResult(); r != nil {
				result = r
			}
		}
		if stream.Err() == nil && progress == 0 {
			t.Error("no progress events")
		}
		return result, stream.Err()
	}

	res, err := run(&supervisorv1.DownloadRequest{Url: fs.URL + "/server.jar", Path: "server.jar",
		Expected: &supervisorv1.Digest{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: hex.EncodeToString(sum[:])}})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetSize() != uint64(len(payload)) || len(res.GetDigests()) != 3 {
		t.Errorf("result = %v", res)
	}
	m, err := env.server.state.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := m.Files["server.jar"]; !ok || e.Digests["sha256"] != hex.EncodeToString(sum[:]) || e.Source == "" {
		t.Errorf("manifest entry = %+v", m.Files)
	}

	_, err = run(&supervisorv1.DownloadRequest{Url: fs.URL + "/bad.jar", Path: "bad.jar",
		Expected: &supervisorv1.Digest{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1, Hex: "00"}})
	if err == nil || !strings.Contains(err.Error(), "sha1 mismatch") {
		t.Errorf("bad digest: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(env.root.Dir, "bad.jar")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("file with bad digest left in place")
	}

	_, err = run(&supervisorv1.DownloadRequest{Url: fs.URL + "/missing", Path: "missing.jar"})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	_, err = run(&supervisorv1.DownloadRequest{Url: "ftp://example.invalid/x", Path: "x"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ftp scheme: %v", err)
	}
	_, err = run(&supervisorv1.DownloadRequest{Url: fs.URL + "/x", Path: ".supervisor/x"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("reserved path: %v", err)
	}
}

func TestRunStreamsOutput(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()
	stream, err := c.Run(ctx, connect.NewRequest(&supervisorv1.RunRequest{
		Command: "sh", Args: []string{"-c", "echo out; echo err 1>&2; pwd; exit 4"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	var result *supervisorv1.RunResult
	for stream.Receive() {
		if o := stream.Msg().GetOutput(); o != nil {
			lines = append(lines, o.GetStream().String()+":"+o.GetText())
		}
		if r := stream.Msg().GetResult(); r != nil {
			result = r
		}
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}
	if result.GetExitCode() != 4 {
		t.Errorf("exit = %d", result.GetExitCode())
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"STREAM_STDOUT:out", "STREAM_STDERR:err", "STREAM_STDOUT:" + env.root.Dir} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}

	// Timeout kills the command.
	stream, err = c.Run(ctx, connect.NewRequest(&supervisorv1.RunRequest{
		Command: "sleep", Args: []string{"10"}, Timeout: durationpb.New(200 * time.Millisecond),
	}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			result = r
		}
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}
	if result.GetExitCode() == 0 {
		t.Error("timed-out run reported success")
	}
}

func TestArchiveContents(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()
	if err := os.MkdirAll(filepath.Join(env.root.Dir, "world", "region"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root.Dir, "world", "region", "r.0.0.mca"), []byte("region"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.root.Dir, "eula.txt"), []byte("eula=true"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Archive(ctx, connect.NewRequest(&supervisorv1.ArchiveRequest{Paths: []string{"world"}, Compression: supervisorv1.Compression_COMPRESSION_NONE}))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for stream.Receive() {
		buf.Write(stream.Msg().GetData())
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name == "world/region/r.0.0.mca" {
			data, _ := io.ReadAll(tr)
			if string(data) != "region" {
				t.Errorf("content = %q", data)
			}
		}
	}
	if strings.Join(names, ",") != "world/,world/region/,world/region/r.0.0.mca" {
		t.Errorf("names = %v", names)
	}

	// Consistent archive without a running process fails cleanly.
	if _, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: echoSpec()})); err != nil {
		t.Fatal(err)
	}
	stream, err = c.Archive(ctx, connect.NewRequest(&supervisorv1.ArchiveRequest{Consistent: true}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if stream.Err() == nil || !strings.Contains(stream.Err().Error(), "not running") {
		t.Errorf("consistent archive while stopped: %v", stream.Err())
	}
}

func TestTunnel(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	c := env.fullClient()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 1024)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(bytes.ToUpper(buf[:n]))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	spec := echoSpec()
	spec.TunnelTargets = map[string]*supervisorv1.TunnelTarget{"echo": {Address: ln.Addr().String()}}
	if _, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: spec})); err != nil {
		t.Fatal(err)
	}

	// Non-loopback targets are rejected at SetLaunch.
	bad := echoSpec()
	bad.TunnelTargets = map[string]*supervisorv1.TunnelTarget{"x": {Address: "10.0.0.1:25585"}}
	if _, err := c.SetLaunch(ctx, connect.NewRequest(&supervisorv1.SetLaunchRequest{Spec: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("non-loopback target accepted: %v", err)
	}

	tun := c.Tunnel(ctx)
	if err := tun.Send(&supervisorv1.TunnelRequest{Message: &supervisorv1.TunnelRequest_Open{Open: &supervisorv1.TunnelOpen{Target: "echo"}}}); err != nil {
		t.Fatal(err)
	}
	if err := tun.Send(&supervisorv1.TunnelRequest{Message: &supervisorv1.TunnelRequest_Data{Data: []byte("hello tunnel")}}); err != nil {
		t.Fatal(err)
	}
	resp, err := tun.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.GetData()) != "HELLO TUNNEL" {
		t.Errorf("tunnel echo = %q", resp.GetData())
	}
	_ = tun.CloseRequest()
	_ = tun.CloseResponse()

	tun = c.Tunnel(ctx)
	if err := tun.Send(&supervisorv1.TunnelRequest{Message: &supervisorv1.TunnelRequest_Open{Open: &supervisorv1.TunnelOpen{Target: "nope"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tun.Receive(); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown target: %v", err)
	}
}

func TestAutostartFromPersistedSpec(t *testing.T) {
	root := newTestRoot(t)
	state, err := NewState(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := echoSpec()
	spec.Autostart = true
	if err := state.SaveLaunch(spec); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := NewManager(root, state, NewConsole(10), logger)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Passthrough = nil
	got := mgr.Spec()
	if got == nil || !got.GetAutostart() || got.GetCommand() != "sh" {
		t.Fatalf("spec not reloaded: %v", got)
	}
	if LaunchHash(got) != LaunchHash(spec) {
		t.Error("hash differs after reload")
	}
	ctx := context.Background()
	if _, err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Stop(ctx, 2*time.Second, false); err != nil {
		t.Fatal(err)
	}
}
