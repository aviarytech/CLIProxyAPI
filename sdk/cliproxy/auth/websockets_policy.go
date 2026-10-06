package auth

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// codexForceWebsocketsDisabled stores the inverse so the zero value matches the
// enabled-by-default upstream.codex.force-websockets setting.
var codexForceWebsocketsDisabled atomic.Bool

// SetCodexForceWebsockets configures whether Codex credentials without an explicit
// websockets setting use the upstream websocket transport. It reports whether the
// effective value changed.
func SetCodexForceWebsockets(enabled bool) bool {
	return codexForceWebsocketsDisabled.Swap(!enabled) != !enabled
}

// CodexForceWebsocketsEnabled reports the current upstream.codex.force-websockets value.
func CodexForceWebsocketsEnabled() bool {
	return !codexForceWebsocketsDisabled.Load()
}

// ExplicitWebsocketsSetting returns the per-credential websockets value from
// attributes or metadata, and whether the credential sets one at all.
func ExplicitWebsocketsSetting(auth *Auth) (enabled bool, ok bool) {
	if auth == nil {
		return false, false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
				return parsed, true
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false, false
	}
	switch v := auth.Metadata["websockets"].(type) {
	case bool:
		return v, true
	case string:
		if parsed, errParse := strconv.ParseBool(strings.TrimSpace(v)); errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// WebsocketsEnabled reports whether requests for auth use the upstream websocket
// transport. An explicit per-credential setting wins; otherwise Codex credentials
// follow upstream.codex.force-websockets and every other provider stays on HTTP.
func WebsocketsEnabled(auth *Auth) bool {
	if enabled, ok := ExplicitWebsocketsSetting(auth); ok {
		return enabled
	}
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && CodexForceWebsocketsEnabled()
}
