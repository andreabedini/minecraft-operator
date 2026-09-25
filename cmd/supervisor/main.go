// Command supervisor is the in-pod agent of the Minecraft operator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"

	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
	"github.com/andreabedini/minecraft-operator/internal/supervisor"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	// "copy-self DEST" copies this binary to DEST. The init container uses it
	// because the scratch image has no cp.
	if len(os.Args) == 3 && os.Args[1] == "copy-self" {
		if err := copySelf(os.Args[2]); err != nil {
			fatal("copy-self: %v", err)
		}
		return
	}
	var (
		dataRoot          = flag.String("data-root", envOr("SUPERVISOR_DATA_ROOT", "/data"), "directory the supervisor manages")
		listen            = flag.String("listen", envOr("SUPERVISOR_LISTEN", ":9800"), "address to serve the API on")
		tokenFile         = flag.String("token-file", envOr("SUPERVISOR_TOKEN_FILE", ""), "file holding the full-access bearer token")
		readOnlyTokenFile = flag.String("readonly-token-file", envOr("SUPERVISOR_READONLY_TOKEN_FILE", ""), "file holding the read-only bearer token")
		noAuth            = flag.Bool("no-auth", false, "disable authentication (local development only)")
		consoleLines      = flag.Int("console-lines", 5000, "console lines kept for replay")
		noAutostart       = flag.Bool("no-autostart", false, "ignore the launch spec's autostart on boot")
		insecureDownloads = flag.Bool("allow-insecure-downloads", false, "permit http:// download URLs")
		logLevel          = flag.String("log-level", envOr("SUPERVISOR_LOG_LEVEL", "info"), "debug, info, warn or error")
		showVersion       = flag.Bool("version", false, "print the version and exit")
		allowHosts        stringList
	)
	flag.Var(&allowHosts, "download-allow-host", "host allowed for Download (repeatable; *.example.com matches subdomains)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fatal("invalid -log-level: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	tokens := supervisor.Tokens{}
	if !*noAuth {
		if *tokenFile == "" {
			fatal("-token-file is required unless -no-auth is set")
		}
		var err error
		if tokens.Full, err = readToken(*tokenFile); err != nil {
			fatal("read token: %v", err)
		}
		if *readOnlyTokenFile != "" {
			if tokens.ReadOnly, err = readToken(*readOnlyTokenFile); err != nil {
				fatal("read read-only token: %v", err)
			}
		}
	}

	root, err := supervisor.NewRoot(*dataRoot)
	if err != nil {
		fatal("data root: %v", err)
	}
	state, err := supervisor.NewState(root)
	if err != nil {
		fatal("state: %v", err)
	}
	console := supervisor.NewConsole(*consoleLines)
	manager, err := supervisor.NewManager(root, state, console, logger)
	if err != nil {
		fatal("manager: %v", err)
	}
	if os.Getpid() == 1 {
		isManaged := func(pid int) bool { return manager.Status().PID == pid }
		if err := supervisor.StartReaper(logger, isManaged); err != nil {
			logger.Warn("could not become subreaper", "error", err)
		}
	}

	srv := supervisor.New(supervisor.Config{
		Version:                version,
		DownloadAllowHosts:     allowHosts,
		AllowInsecureDownloads: *insecureDownloads,
	}, root, state, console, manager, logger)

	var opts []connect.HandlerOption
	if *noAuth {
		logger.Warn("authentication disabled")
	} else {
		opts = append(opts, connect.WithInterceptors(supervisor.NewAuthInterceptor(tokens)))
	}
	mux := http.NewServeMux()
	mux.Handle(supervisorv1connect.NewSupervisorServiceHandler(srv, opts...))
	reflector := grpcreflect.NewStaticReflector(supervisorv1connect.SupervisorServiceName)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(supervisorv1connect.SupervisorServiceName)))

	// gRPC needs HTTP/2; inside the pod network it is cleartext (h2c).
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if spec := manager.Spec(); spec != nil && spec.GetAutostart() && !*noAutostart {
		if pid, err := manager.Start(ctx); err != nil {
			logger.Error("autostart failed", "error", err)
		} else {
			logger.Info("autostarted", "pid", pid)
		}
	}

	go func() {
		logger.Info("serving", "addr", *listen, "version", version, "data_root", root.Dir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if code, err := manager.Stop(shutdownCtx, 0, false); err != nil {
		logger.Error("stop on shutdown failed", "error", err)
	} else if manager.Spec() != nil {
		logger.Info("process stopped", "code", code)
	}
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpCancel()
	_ = httpServer.Shutdown(httpCtx)
}

func copySelf(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "supervisor: "+format+"\n", args...)
	os.Exit(2)
}
