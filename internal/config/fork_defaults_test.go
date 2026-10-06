package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexForceWebsocketsDefaultsToEnabled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "omitted", raw: "upstream: {codex: {response-steering: false}}\n", want: true},
		{name: "upstream true", raw: "upstream: {codex: {force-websockets: true}}\n", want: true},
		{name: "upstream false", raw: "upstream: {codex: {force-websockets: false}}\n", want: false},
		{name: "legacy false", raw: "codex: {force-websockets: false}\n", want: false},
		{name: "historical oauth false", raw: "oauth: {providers: {codex: {force-websockets: false}}}\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tc.raw))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := cfg.Codex.ForceWebsocketsEnabled(); got != tc.want {
				t.Fatalf("ForceWebsocketsEnabled() = %t, want %t", got, tc.want)
			}
			// The setting is shared upstream behavior, so API-key credentials see it too.
			if got := cfg.ForAPIKey().Codex.ForceWebsocketsEnabled(); got != tc.want {
				t.Fatalf("ForAPIKey().ForceWebsocketsEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestCodexForceWebsocketsMigratesToUpstreamPath(t *testing.T) {
	migrated, _, errMigrate := NormalizeConfigLayout([]byte("codex: {force-websockets: false}\n"), true)
	if errMigrate != nil {
		t.Fatal(errMigrate)
	}
	var doc yaml.Node
	if errUnmarshal := yaml.Unmarshal(migrated, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	value := yamlPath(doc.Content[0], "upstream.codex.force-websockets")
	if value == nil || value.Value != "false" {
		t.Fatalf("migrated layout does not carry upstream.codex.force-websockets=false: %s", migrated)
	}
	if strings.Contains(string(migrated), "oauth:") && yamlPath(doc.Content[0], "oauth.providers.codex.force-websockets") != nil {
		t.Fatalf("force-websockets was scoped to OAuth credentials: %s", migrated)
	}
}

func TestRoutingSessionAffinityDefaultsToEnabled(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "omitted", raw: "routing: {strategy: round-robin}\n", want: true},
		{name: "true", raw: "routing: {session-affinity: true}\n", want: true},
		{name: "false", raw: "routing: {session-affinity: false}\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tc.raw))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := cfg.Routing.SessionAffinityEnabled(); got != tc.want {
				t.Fatalf("SessionAffinityEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestExplicitFalseDefaultsSurviveYAMLRoundTrip(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("routing: {session-affinity: false}\nupstream: {codex: {force-websockets: false}}\napi-keys: {codex: [{base-url: https://example.invalid, keys: [{api-key: k, websockets: false}]}]}\n"))
	if errParse != nil {
		t.Fatal(errParse)
	}
	out, errMarshal := yaml.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	reparsed, errReparse := ParseConfigBytes(out)
	if errReparse != nil {
		t.Fatal(errReparse)
	}
	if reparsed.Routing.SessionAffinityEnabled() || reparsed.Codex.ForceWebsocketsEnabled() {
		t.Fatalf("explicit false settings were lost on round trip:\n%s", out)
	}
	if len(reparsed.CodexKey) != 1 || reparsed.CodexKey[0].Websockets == nil || *reparsed.CodexKey[0].Websockets {
		t.Fatalf("explicit per-key websockets: false was lost on round trip:\n%s", out)
	}
}
