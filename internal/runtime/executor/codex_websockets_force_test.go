package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexForceCompletedEvent = `{"type":"response.completed","response":{"id":"resp-force","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`

type codexForceFrame struct {
	conn    int
	payload []byte
}

// codexForceRecorder is a websocket upstream that records every handshake and request frame.
// respond returns the frames to send for the n-th request on connection conn.
type codexForceRecorder struct {
	mu         sync.Mutex
	handshakes []http.Header
	frames     []codexForceFrame
	respond    func(conn int, n int) []string
}

func (r *codexForceRecorder) handshakeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.handshakes)
}

func (r *codexForceRecorder) snapshotFrames() []codexForceFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]codexForceFrame(nil), r.frames...)
}

func newCodexForceServer(t *testing.T, rec *codexForceRecorder) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.handshakes = append(rec.handshakes, r.Header.Clone())
		connIndex := len(rec.handshakes) - 1
		rec.mu.Unlock()
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		for n := 0; ; n++ {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			rec.mu.Lock()
			rec.frames = append(rec.frames, codexForceFrame{conn: connIndex, payload: payload})
			rec.mu.Unlock()
			responses := []string{codexForceCompletedEvent}
			if rec.respond != nil {
				responses = rec.respond(connIndex, n)
			}
			for _, frame := range responses {
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newIsolatedCodexWebsocketsExecutor(t *testing.T, cfg *config.Config) *CodexWebsocketsExecutor {
	t.Helper()
	exec := NewCodexWebsocketsExecutor(cfg)
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	t.Cleanup(func() { exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
	return exec
}

func pooledSessionCount(exec *CodexWebsocketsExecutor) int {
	store := exec.sessionStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for key, sess := range store.sessions {
		if sess != nil && sess.pooled && strings.HasPrefix(key, codexHTTPPooledSessionPrefix) {
			count++
		}
	}
	return count
}

func codexForceOptions(sessionID string) cliproxyexecutor.Options {
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	if sessionID != "" {
		opts.Metadata = map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: sessionID}
	}
	return opts
}

func codexForceRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)}
}

func TestCodexAutoExecutorWebsocketRoutingMatrix(t *testing.T) {
	enabled, disabled := true, false
	forceSettings := []struct {
		name  string
		value *bool
		on    bool
	}{
		{name: "force omitted", value: nil, on: true},
		{name: "force true", value: &enabled, on: true},
		{name: "force false", value: &disabled, on: false},
	}
	authSettings := []struct {
		name     string
		attrs    map[string]string
		explicit *bool
	}{
		{name: "auth websockets unset"},
		{name: "auth websockets true", attrs: map[string]string{"websockets": "true"}, explicit: &enabled},
		{name: "auth websockets false", attrs: map[string]string{"websockets": "false"}, explicit: &disabled},
	}
	for _, force := range forceSettings {
		for _, downstreamWS := range []bool{true, false} {
			for _, authSetting := range authSettings {
				name := force.name + "/" + map[bool]string{true: "downstream websocket", false: "downstream http"}[downstreamWS] + "/" + authSetting.name
				t.Run(name, func(t *testing.T) {
					exec := NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{ForceWebsockets: force.value}})
					ctx := context.Background()
					if downstreamWS {
						ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
					}
					auth := &cliproxyauth.Auth{ID: "codex-matrix", Provider: "codex", Attributes: authSetting.attrs}

					authUsesWebsockets := force.on
					if authSetting.explicit != nil {
						authUsesWebsockets = *authSetting.explicit
					}
					want := authUsesWebsockets && (downstreamWS || force.on)
					if got := exec.useWebsockets(ctx, auth, cliproxyexecutor.Options{}); got != want {
						t.Fatalf("useWebsockets() = %t, want %t", got, want)
					}
				})
			}
		}
	}
}

func TestCodexAutoExecutorKeepsImageEndpointsOnHTTP(t *testing.T) {
	exec := NewCodexAutoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "codex-image", Provider: "codex"}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(codexOpenAIImageSourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}
	if exec.useWebsockets(context.Background(), auth, opts) {
		t.Fatal("image endpoint request was routed to the websocket executor")
	}
	if !exec.useWebsockets(context.Background(), auth, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}) {
		t.Fatal("Responses request was not routed to the websocket executor by default")
	}
}

func TestCodexAutoExecutorForcedWebsocketServesHTTPDownstreamStream(t *testing.T) {
	rec := &codexForceRecorder{}
	server := newCodexForceServer(t, rec)

	alias := t.Name()
	capture := &codexResponseModelUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })

	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	t.Cleanup(func() { exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })

	ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
	result, errStream := exec.ExecuteStream(ctx, codexAPIKeyTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","previous_response_id":"resp-0","generate":false,"parallel_tool_calls":true,"stream_options":{"include_usage":true,"reasoning_summary_delivery":"sequential_cutoff"}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	output, errDrain := drainChunks(result)
	if errDrain != nil {
		t.Fatalf("stream error = %v", errDrain)
	}
	if !strings.Contains(output, "response.completed") {
		t.Fatalf("stream output = %q, want the translated completion", output)
	}

	if got := rec.handshakeCount(); got != 1 {
		t.Fatalf("websocket handshakes = %d, want 1", got)
	}
	frames := rec.snapshotFrames()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	body := frames[0].payload
	if gjson.GetBytes(body, "type").String() != "response.create" {
		t.Fatalf("frame type = %q, want response.create: %s", gjson.GetBytes(body, "type").String(), body)
	}
	for _, path := range []string{"previous_response_id", "generate", "stream_options.include_usage", "parallel_tool_calls"} {
		if gjson.GetBytes(body, path).Exists() {
			t.Fatalf("HTTP client request kept %s over the websocket: %s", path, body)
		}
	}
	if got := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery").String(); got != "sequential_cutoff" {
		t.Fatalf("reasoning_summary_delivery = %q, want sequential_cutoff: %s", got, body)
	}

	record := capture.await(t)
	if record.Failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 5 {
		t.Fatalf("usage record = %+v, want 3 in / 2 out", record)
	}
}

func TestCodexWebsocketsHTTPDownstreamCleanupPrecedesPayloadConfig(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			rec := &codexForceRecorder{}
			server := newCodexForceServer(t, rec)
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gpt-5.5", Protocol: "codex"}},
				Params: map[string]any{"parallel_tool_calls": true},
			}}}}
			exec := newIsolatedCodexWebsocketsExecutor(t, cfg)
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello","parallel_tool_calls":false}`)}
			opts := codexForceOptions("")
			if stream {
				result, errStream := exec.ExecuteStream(context.Background(), codexAPIKeyTestAuth(server.URL), req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				if _, errDrain := drainChunks(result); errDrain != nil {
					t.Fatalf("stream error = %v", errDrain)
				}
			} else if _, errExecute := exec.Execute(context.Background(), codexAPIKeyTestAuth(server.URL), req, opts); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			frames := rec.snapshotFrames()
			if len(frames) != 1 {
				t.Fatalf("frames = %d, want 1", len(frames))
			}
			if got := gjson.GetBytes(frames[0].payload, "parallel_tool_calls"); !got.Exists() || !got.Bool() {
				t.Fatalf("configured parallel_tool_calls was not the final value: %s", frames[0].payload)
			}
		})
	}
}

func TestCodexWebsocketsReusesSocketForHTTPConversationOnSameAuth(t *testing.T) {
	rec := &codexForceRecorder{}
	server := newCodexForceServer(t, rec)
	exec := newIsolatedCodexWebsocketsExecutor(t, &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})

	authA := codexAPIKeyTestAuth(server.URL)
	authA.ID = "codex-pool-a"
	authB := codexAPIKeyTestAuth(server.URL)
	authB.ID = "codex-pool-b"
	execute := func(auth *cliproxyauth.Auth, sessionID string) {
		t.Helper()
		if _, errExecute := exec.Execute(context.Background(), auth, codexForceRequest(), codexForceOptions(sessionID)); errExecute != nil {
			t.Fatalf("Execute(%s, %q) error = %v", auth.ID, sessionID, errExecute)
		}
	}

	execute(authA, "conversation-1")
	execute(authA, "conversation-1")
	if got := rec.handshakeCount(); got != 1 {
		t.Fatalf("handshakes after two requests of one conversation = %d, want 1", got)
	}
	frames := rec.snapshotFrames()
	if len(frames) != 2 || frames[0].conn != 0 || frames[1].conn != 0 {
		t.Fatalf("frames = %+v, want both requests on connection 0", frames)
	}
	for _, frame := range frames {
		if gjson.GetBytes(frame.payload, "previous_response_id").Exists() {
			t.Fatalf("reused socket chained requests with previous_response_id: %s", frame.payload)
		}
		if gjson.GetBytes(frame.payload, "input.0.content.0.text").String() != "hello" {
			t.Fatalf("reused socket did not carry the full input: %s", frame.payload)
		}
	}

	// The same conversation on another credential, as after an affinity failover, gets its own socket.
	execute(authB, "conversation-1")
	if got := rec.handshakeCount(); got != 2 {
		t.Fatalf("handshakes after switching credential = %d, want 2", got)
	}
	if got := rec.snapshotFrames()[2].conn; got != 1 {
		t.Fatalf("credential B request used connection %d, want 1", got)
	}
	// Returning to the first credential reuses its socket again.
	execute(authA, "conversation-1")
	if got := rec.snapshotFrames()[3].conn; got != 0 || rec.handshakeCount() != 2 {
		t.Fatalf("credential A request used connection %d with %d handshakes, want connection 0 and 2 handshakes", got, rec.handshakeCount())
	}

	execute(authA, "conversation-2")
	if got := rec.handshakeCount(); got != 3 {
		t.Fatalf("handshakes after a new conversation = %d, want 3", got)
	}
	if got := pooledSessionCount(exec); got != 3 {
		t.Fatalf("pooled sessions = %d, want 3", got)
	}

	// Requests without a session identity keep using one-shot sockets.
	execute(authA, "")
	execute(authA, "")
	if got := rec.handshakeCount(); got != 5 {
		t.Fatalf("handshakes after two sessionless requests = %d, want 5", got)
	}
	if got := pooledSessionCount(exec); got != 3 {
		t.Fatalf("pooled sessions after sessionless requests = %d, want 3", got)
	}
}

func TestCodexWebsocketsPooledStreamReusesSocket(t *testing.T) {
	rec := &codexForceRecorder{}
	server := newCodexForceServer(t, rec)
	exec := newIsolatedCodexWebsocketsExecutor(t, &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := codexAPIKeyTestAuth(server.URL)
	auth.ID = "codex-pool-stream"

	for i := 0; i < 2; i++ {
		result, errStream := exec.ExecuteStream(context.Background(), auth, codexForceRequest(), codexForceOptions("conversation-stream"))
		if errStream != nil {
			t.Fatalf("ExecuteStream() #%d error = %v", i, errStream)
		}
		if _, errDrain := drainChunks(result); errDrain != nil {
			t.Fatalf("stream #%d error = %v", i, errDrain)
		}
	}
	if got := rec.handshakeCount(); got != 1 {
		t.Fatalf("handshakes = %d, want 1", got)
	}
	if frames := rec.snapshotFrames(); len(frames) != 2 || frames[1].conn != 0 {
		t.Fatalf("frames = %+v, want two requests on connection 0", frames)
	}
}

func TestCodexWebsocketsPooledSocketRedialsWhenHandshakeChanges(t *testing.T) {
	rec := &codexForceRecorder{}
	server := newCodexForceServer(t, rec)
	exec := newIsolatedCodexWebsocketsExecutor(t, &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := codexAPIKeyTestAuth(server.URL)
	auth.ID = "codex-pool-turns"

	for _, turn := range []string{"turn-1", "turn-1", "turn-2"} {
		opts := codexForceOptions("conversation-turns")
		opts.Headers = http.Header{"X-Codex-Turn-Metadata": []string{turn}}
		if _, errExecute := exec.Execute(context.Background(), auth, codexForceRequest(), opts); errExecute != nil {
			t.Fatalf("Execute(%s) error = %v", turn, errExecute)
		}
	}
	if got := rec.handshakeCount(); got != 2 {
		t.Fatalf("handshakes = %d, want 2", got)
	}
	frames := rec.snapshotFrames()
	if len(frames) != 3 || frames[0].conn != 0 || frames[1].conn != 0 || frames[2].conn != 1 {
		t.Fatalf("frames = %+v, want turn-1 on connection 0 and turn-2 on connection 1", frames)
	}
	if got := rec.handshakes[1].Get("X-Codex-Turn-Metadata"); got != "turn-2" {
		t.Fatalf("second handshake turn metadata = %q, want turn-2", got)
	}
	if got := pooledSessionCount(exec); got != 1 {
		t.Fatalf("pooled sessions = %d, want 1", got)
	}
}

func TestCodexWebsocketsPooledSessionNeverQueuesAndEvictsWhenIdle(t *testing.T) {
	exec := newIsolatedCodexWebsocketsExecutor(t, &config.Config{})
	ctx := context.Background()
	key := codexHTTPPooledSessionKey(ctx, codexForceOptions("conversation-busy"), "codex-pool-busy")
	if key == "" {
		t.Fatal("pooled session key is empty for an HTTP request with a session identity")
	}
	if codexHTTPPooledSessionKey(cliproxyexecutor.WithDownstreamWebsocket(ctx), codexForceOptions("conversation-busy"), "codex-pool-busy") != "" {
		t.Fatal("downstream websocket requests must not use pooled HTTP sessions")
	}
	if codexHTTPPooledSessionKey(ctx, codexForceOptions("conversation-busy"), "codex-pool-other") == key {
		t.Fatal("pooled session keys of different credentials collide")
	}

	held := exec.acquirePooledSession(key)
	if held == nil {
		t.Fatal("acquirePooledSession() = nil for an idle key")
	}
	if busy := exec.acquirePooledSession(key); busy != nil {
		t.Fatal("acquirePooledSession() returned a session already in use")
	}
	held.reqMu.Unlock()
	exec.evictIdlePooledSession(held)
	if got := pooledSessionCount(exec); got != 0 {
		t.Fatalf("pooled sessions after releasing a socketless session = %d, want 0", got)
	}
	if fresh := exec.acquirePooledSession(key); fresh == nil || fresh == held {
		t.Fatal("acquirePooledSession() did not create a fresh session after eviction")
	} else {
		fresh.reqMu.Unlock()
	}
}

func TestCodexWebsocketsPooledSessionDropsSocketOnBootstrapOverload(t *testing.T) {
	rec := &codexForceRecorder{respond: func(conn int, n int) []string {
		if conn == 0 && n == 1 {
			return []string{codexCreatedEvent, codexInProgressEvent, codexOverloadEvent}
		}
		return []string{codexForceCompletedEvent}
	}}
	server := newCodexForceServer(t, rec)
	cfg := codexBufferingConfig(true)
	cfg.DisableImageGeneration = config.DisableImageGenerationAll
	exec := newIsolatedCodexWebsocketsExecutor(t, cfg)
	auth := codexAPIKeyTestAuth(server.URL)
	auth.ID = "codex-pool-overload"
	stream := func() error {
		result, errStream := exec.ExecuteStream(context.Background(), auth, codexForceRequest(), codexForceOptions("conversation-overload"))
		if errStream != nil {
			return errStream
		}
		_, errDrain := drainChunks(result)
		return errDrain
	}

	if errFirst := stream(); errFirst != nil {
		t.Fatalf("first request error = %v", errFirst)
	}
	errOverload := stream()
	if errOverload == nil {
		t.Fatal("expected the overload rejection to fail the attempt")
	}
	if got := statusCodeFromTestError(t, errOverload); got != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := pooledSessionCount(exec); got != 0 {
		t.Fatalf("pooled sessions after overload = %d, want the dropped socket evicted", got)
	}
	if errThird := stream(); errThird != nil {
		t.Fatalf("request after overload error = %v", errThird)
	}
	frames := rec.snapshotFrames()
	if len(frames) != 3 || frames[2].conn != 1 {
		t.Fatalf("frames = %+v, want the request after overload on a fresh connection", frames)
	}
}

func TestCodexWebsocketsPublishesImageToolUsage(t *testing.T) {
	completed := `{"type":"response.completed","response":{"id":"resp-image","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"tool_usage":{"image_gen":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}}`
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			rec := &codexForceRecorder{respond: func(int, int) []string { return []string{completed} }}
			server := newCodexForceServer(t, rec)
			alias := t.Name()
			capture := &codexResponseModelUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })

			exec := newIsolatedCodexWebsocketsExecutor(t, &config.Config{})
			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			if stream {
				result, errStream := exec.ExecuteStream(ctx, codexAPIKeyTestAuth(server.URL), codexForceRequest(), codexForceOptions(""))
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				if _, errDrain := drainChunks(result); errDrain != nil {
					t.Fatalf("stream error = %v", errDrain)
				}
			} else if _, errExecute := exec.Execute(ctx, codexAPIKeyTestAuth(server.URL), codexForceRequest(), codexForceOptions("")); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}

			main := capture.await(t)
			if main.Detail.TotalTokens != 5 {
				t.Fatalf("main usage = %+v, want 5 total tokens", main.Detail)
			}
			image := capture.await(t)
			if image.Model != codexDefaultImageToolModel || image.Detail.TotalTokens != 30 {
				t.Fatalf("image usage record = model %q detail %+v, want %s with 30 total tokens", image.Model, image.Detail, codexDefaultImageToolModel)
			}
		})
	}
}

func TestCodexWebsocketPromptCacheKeepsChatCompletionsCacheKey(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","messages":[],"prompt_cache_key":"chat-cache"}`)}
	body, headers, errCache := applyCodexPromptCacheHeadersWithContext(context.Background(), sdktranslator.FormatOpenAI, req, []byte(`{"model":"gpt-5.5"}`))
	if errCache != nil {
		t.Fatalf("applyCodexPromptCacheHeadersWithContext() error = %v", errCache)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "chat-cache" {
		t.Fatalf("prompt_cache_key = %q, want chat-cache", got)
	}
	if got := headers.Get("Conversation_id"); got != "chat-cache" {
		t.Fatalf("Conversation_id = %q, want chat-cache", got)
	}
}
