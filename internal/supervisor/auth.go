package supervisor

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
)

// Tokens are the two bearer tokens the supervisor accepts. An empty token
// disables that level.
type Tokens struct {
	Full     string
	ReadOnly string
}

// readOnlyProcedures may be called with the read-only token.
var readOnlyProcedures = map[string]bool{
	supervisorv1connect.SupervisorServiceStatusProcedure:    true,
	supervisorv1connect.SupervisorServiceListFilesProcedure: true,
	supervisorv1connect.SupervisorServiceReadFileProcedure:  true,
	supervisorv1connect.SupervisorServiceArchiveProcedure:   true,
}

var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("missing or invalid bearer token"))
var errPermissionDenied = connect.NewError(connect.CodePermissionDenied, errors.New("read-only token cannot call this procedure"))

type authInterceptor struct {
	tokens Tokens
}

// NewAuthInterceptor checks the Authorization header on every call.
func NewAuthInterceptor(tokens Tokens) connect.Interceptor {
	return &authInterceptor{tokens: tokens}
}

func (a *authInterceptor) check(procedure string, header http.Header) error {
	raw := header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return errUnauthenticated
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return errUnauthenticated
	}
	if a.tokens.Full != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.tokens.Full)) == 1 {
		return nil
	}
	if a.tokens.ReadOnly != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.tokens.ReadOnly)) == 1 {
		if readOnlyProcedures[procedure] {
			return nil
		}
		return errPermissionDenied
	}
	return errUnauthenticated
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		if err := a.check(req.Spec().Procedure, req.Header()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.check(conn.Spec().Procedure, conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
