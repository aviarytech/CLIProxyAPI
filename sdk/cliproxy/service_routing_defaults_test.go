package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func sessionAffinityPtr(value bool) *bool {
	return &value
}

func TestNormalizedRoutingRuntimeStateSessionAffinityDefaults(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{name: "nil config", cfg: nil, want: true},
		{name: "omitted", cfg: &config.Config{}, want: true},
		{name: "explicit true", cfg: &config.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: sessionAffinityPtr(true)}}, want: true},
		{name: "explicit false", cfg: &config.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: sessionAffinityPtr(false)}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := normalizedRoutingRuntimeState(tc.cfg)
			if state.sessionAffinity != tc.want {
				t.Fatalf("sessionAffinity = %t, want %t", state.sessionAffinity, tc.want)
			}
			_, isAffinity := newRoutingSelector(state).(*coreauth.SessionAffinitySelector)
			if isAffinity != tc.want {
				t.Fatalf("selector is SessionAffinitySelector = %t, want %t", isAffinity, tc.want)
			}
		})
	}
}
