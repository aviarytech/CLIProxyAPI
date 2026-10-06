package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/sjson"
)

func setCodexForceWebsocketsForTest(t *testing.T, enabled bool) {
	t.Helper()
	previous := CodexForceWebsocketsEnabled()
	SetCodexForceWebsockets(enabled)
	t.Cleanup(func() { SetCodexForceWebsockets(previous) })
}

func TestWebsocketsEnabledFollowsExplicitSettingThenCodexForce(t *testing.T) {
	cases := []struct {
		name  string
		auth  *Auth
		force bool
		want  bool
	}{
		{name: "nil auth", auth: nil, force: true, want: false},
		{name: "codex unset forced", auth: &Auth{Provider: "codex"}, force: true, want: true},
		{name: "codex unset not forced", auth: &Auth{Provider: "codex"}, force: false, want: false},
		{name: "codex attribute false forced", auth: &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "false"}}, force: true, want: false},
		{name: "codex attribute true not forced", auth: &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "true"}}, force: false, want: true},
		{name: "codex metadata false forced", auth: &Auth{Provider: "codex", Metadata: map[string]any{"websockets": false}}, force: true, want: false},
		{name: "codex metadata string true not forced", auth: &Auth{Provider: "codex", Metadata: map[string]any{"websockets": "true"}}, force: false, want: true},
		{name: "codex invalid attribute falls through to metadata", auth: &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "maybe"}, Metadata: map[string]any{"websockets": false}}, force: true, want: false},
		{name: "xai unset forced", auth: &Auth{Provider: "xai"}, force: true, want: false},
		{name: "xai attribute true", auth: &Auth{Provider: "xai", Attributes: map[string]string{"websockets": "true"}}, force: false, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setCodexForceWebsocketsForTest(t, tc.force)
			if got := WebsocketsEnabled(tc.auth); got != tc.want {
				t.Fatalf("WebsocketsEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestManagerSetConfigTogglesCodexForceWebsocketsAndRebuildsScheduler(t *testing.T) {
	setCodexForceWebsocketsForTest(t, true)
	const model = "codex-force-websockets-model"
	explicitWS := &Auth{ID: "codex-force-ws-explicit", Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	unset := &Auth{ID: "codex-force-ws-unset", Provider: "codex"}
	optOut := &Auth{ID: "codex-force-ws-opt-out", Provider: "codex", Attributes: map[string]string{"websockets": "false"}}
	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{explicitWS, unset, optOut} {
		reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, auth := range []*Auth{explicitWS, unset, optOut} {
			reg.UnregisterClient(auth.ID)
		}
	})

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&refreshMockExecutor{id: "codex"})
	manager.SetConfig(&internalconfig.Config{})
	for _, auth := range []*Auth{explicitWS, unset, optOut} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	pickSet := func() map[string]bool {
		t.Helper()
		ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
		seen := make(map[string]bool)
		for i := 0; i < 6; i++ {
			picked, _, errPick := manager.pickNext(ctx, "codex", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			seen[picked.ID] = true
		}
		return seen
	}

	forced := pickSet()
	if !forced[explicitWS.ID] || !forced[unset.ID] || forced[optOut.ID] {
		t.Fatalf("forced websocket picks = %v, want explicit and unset credentials only", forced)
	}

	disabled := false
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{ForceWebsockets: &disabled}})
	if CodexForceWebsocketsEnabled() {
		t.Fatal("CodexForceWebsocketsEnabled() = true after explicit false config")
	}
	unforced := pickSet()
	if !unforced[explicitWS.ID] || unforced[unset.ID] || unforced[optOut.ID] {
		t.Fatalf("unforced websocket picks = %v, want only the explicitly enabled credential", unforced)
	}
}

func TestSanitizeDownstreamWebsocketFallbackRequestKeepsGenerateForForcedCodexAuth(t *testing.T) {
	setCodexForceWebsocketsForTest(t, true)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	payload := []byte(`{"generate":false,"input":[]}`)

	forced := sanitizeDownstreamWebsocketFallbackRequest(ctx, &Auth{Provider: "codex"}, cliproxyexecutor.Request{Payload: payload})
	if string(forced.Payload) != string(payload) {
		t.Fatalf("forced websocket payload = %s, want generate preserved", forced.Payload)
	}

	optOut := sanitizeDownstreamWebsocketFallbackRequest(ctx, &Auth{Provider: "codex", Attributes: map[string]string{"websockets": "false"}}, cliproxyexecutor.Request{Payload: payload})
	want, _ := sjson.DeleteBytes(payload, "generate")
	if string(optOut.Payload) != string(want) {
		t.Fatalf("HTTP fallback payload = %s, want %s", optOut.Payload, want)
	}
}
