// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

// CodexAutoExecutor routes Codex Responses requests to the websocket transport when the
// selected auth uses websockets (see codexWebsocketsEnabled) and either:
//  1. The downstream transport is websocket, or
//  2. upstream.codex.force-websockets is enabled (the default), which also covers
//     HTTP/SSE downstream requests.
//
// Everything else, including image endpoints, token counting and raw HTTP requests,
// uses the HTTP implementation.
type CodexAutoExecutor struct {
	cfg      *config.Config
	httpExec *CodexExecutor
	wsExec   *CodexWebsocketsExecutor
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		cfg:      cfg,
		httpExec: NewCodexExecutor(cfg),
		wsExec:   NewCodexWebsocketsExecutor(cfg),
	}
}

// useWebsockets reports whether a Responses request should use the websocket executor.
func (e *CodexAutoExecutor) useWebsockets(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) bool {
	if !codexWebsocketsEnabled(e.cfg, auth) {
		return false
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return true
	}
	if !codexForceWebsockets(e.cfg) {
		return false
	}
	// The images endpoints have no websocket equivalent.
	return !isCodexOpenAIImageRequest(opts)
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if e.useWebsockets(ctx, auth, opts) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if e.useWebsockets(ctx, auth, opts) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil || e.wsExec == nil {
		return
	}
	e.wsExec.CloseExecutionSession(sessionID)
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

// codexForceWebsockets reports upstream.codex.force-websockets, which defaults to enabled.
func codexForceWebsockets(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	return cfg.Codex.ForceWebsocketsEnabled()
}

// codexWebsocketsEnabled reports whether auth uses the upstream websocket transport.
// An explicit websockets attribute or metadata value wins; otherwise the credential
// follows upstream.codex.force-websockets.
func codexWebsocketsEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if enabled, ok := cliproxyauth.ExplicitWebsocketsSetting(auth); ok {
		return enabled
	}
	return codexForceWebsockets(cfg)
}

// SupportsApplyPatch requires both selectable transports to support the tool.
func (e *CodexAutoExecutor) SupportsApplyPatch() bool {
	return e != nil && e.httpExec != nil && e.wsExec != nil && e.httpExec.SupportsApplyPatch() && e.wsExec.SupportsApplyPatch()
}
