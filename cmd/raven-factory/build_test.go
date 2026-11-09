package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-factory-core/pkg/codeclink"
	"github.com/logoscore/logos-golang-factory-core/pkg/factorytest"
	"github.com/logoscore/logos-golang-module-core/pkg/mgmt"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// request is a build of a script with two HTTP reverse-call connections.
func request(t *testing.T) protocol.BuildRequest {
	t.Helper()
	material, _ := json.Marshal([]byte("raven key"))
	conn := func(name string, params, values map[string]any) protocol.BuildConnection {
		return protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Format: protocol.ChannelEndpointExportV2, Name: name,
			Channel: protocol.ExportChannel{ModuleName: httpChannel, SchemaVersion: 3}, Kind: protocol.KindReverseCall, Params: params},
			Values: values}
	}
	return protocol.BuildRequest{
		BuildID: "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f", Target: targetScript, Platform: "windows/any", SchemaVersion: 1,
		Values:   map[string]any{"greeting": "it's ‘quoted’"},
		Commands: []string{"echo"},
		Connections: []protocol.BuildConnection{
			conn("edge", map[string]any{"base_url": "https://edge.example.com", "extra_base_urls": []any{"https://b.example.com"},
				"user_agent": "Mozilla/5.0 'x'", "headers": map[string]any{"X-B": "2", "X-A": "1"}, "request_timeout": "40s",
				"checkin_interval": "1m30s", "checkin_jitter": float64(25), "tls_verify": true},
				map[string]any{"method": "PUT", "path": "/cdn/"}),
			conn("second", map[string]any{"base_url": "http://10.0.0.5:8080"}, nil),
		},
		Crypto: &protocol.BuildCrypto{Suite: suiteName, Material: material}, Envelope: envelopeName,
	}
}

func TestSchema(t *testing.T) {
	if err := protocol.ValidateBuildSchema(buildSchema); err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateCodecOwner(buildSchema, "raven"); err != nil {
		t.Fatal(err)
	}
	if !buildSchema.Translates() || len(buildSchema.Compression) != 0 {
		t.Fatal("Raven translates both layers and packs nothing")
	}
	c := buildSchema.TargetCodec(buildSchema.Targets[0])
	if c.Suite != "raven.raven" || c.Envelope != "raven.clixml" || len(c.Layers()) != 2 || c.Agentless() {
		t.Fatalf("codec %+v", c)
	}
	if _, err := mgmt.New(manifest()); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// The script's ops are the schema's.
	for name, op := range map[string]int{"OpHello": opHello, "OpEcho": opEcho} {
		if !strings.Contains(scriptTemplate, fmt.Sprintf("$%s = %d\n", name, op)) {
			t.Errorf("raven.ps1 and the schema disagree on %s", name)
		}
	}
	ops := map[string]int{}
	for _, cmd := range buildSchema.Commands {
		ops[cmd.ID] = cmd.Op
	}
	if len(ops) != 2 || ops["hello"] != opHello || ops["echo"] != opEcho {
		t.Errorf("the schema's commands %v", ops)
	}
}

func TestBuild(t *testing.T) {
	store := factorytest.NewStore(t)
	out := factorytest.Run(t, buildSchema, &raven{version: "v9"}, store, request(t))
	if out.Result.Status != protocol.BuildSucceeded || out.Result.Artifact == nil || out.Result.Artifact.Filename != "raven-5b0c4f6e.ps1" {
		t.Fatalf("result: %+v", out.Result)
	}
	script := string(out.Artifact)
	if !strings.HasPrefix(script, "\ufeff# Raven minion") {
		t.Errorf("the script does not start with UTF-8's byte order mark: %q", script[:min(20, len(script))])
	}
	for _, want := range []string{
		"$RavenBuild = '5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f'",
		"$RavenVersion = 'v9'",
		"$RavenGreeting = 'it''s ‘‘quoted’’'",
		"$RavenOps = @(2)",
		"@{ Place = 1; Urls = @('https://edge.example.com', 'https://b.example.com'); Method = 'PUT'; Path = '/cdn/'; " +
			"UserAgent = 'Mozilla/5.0 ''x'''; Headers = @{ 'X-A' = '1'; 'X-B' = '2' }; TimeoutMs = 40000; IntervalMs = 90000; Jitter = 25; TlsVerify = $true },",
		"@{ Place = 2; Urls = @('http://10.0.0.5:8080'); Method = 'POST'; Path = '/'; UserAgent = ''; Headers = @{  }; " +
			"TimeoutMs = 40000; IntervalMs = 60000; Jitter = 20; TlsVerify = $false }",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
	if strings.Contains(script, "__") && strings.Contains(script, "__BUILD_ID__") {
		t.Error("a placeholder left")
	}

	for name, mutate := range map[string]func(*protocol.BuildRequest){
		"no_keys": func(r *protocol.BuildRequest) { r.Crypto = nil },
		"bad_connection": func(r *protocol.BuildRequest) {
			r.Connections[1].Endpoint.Params["checkin_interval"] = "soon"
		},
	} {
		r := request(t)
		mutate(&r)
		if _, _, err := render(r, "v9"); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCodec(t *testing.T) {
	ctx := context.Background()
	// Keys for a build; no stage.
	k, err := ravenCodec{}.Keys(ctx, codeclink.KeysFor{})
	if err != nil || len(k.Material) != 32 || k.Wrapped != nil {
		t.Fatalf("keys %+v %v", k, err)
	}
	if _, err := (ravenCodec{}).Keys(ctx, codeclink.KeysFor{Stage: true, StageKey: []byte("k")}); err == nil {
		t.Fatal("a stage's keys")
	}
	// What is not CLIXML is not a message.
	a := codeclink.Answer(ctx, ravenCodec{}, protocol.FactoryCall{Type: protocol.FactoryInbound, Request: 1, Build: make([]byte, 16),
		ID: make([]byte, 16), Data: []byte("not xml"), Deadline: 1, Layers: []int{protocol.CodecLayerSuite, protocol.CodecLayerEnvelope},
		Keys: [][]byte{[]byte("k")}})
	if a.Failure == nil || a.Failure.Kind != protocol.CodecFailNotMessage {
		t.Fatalf("garbage: %+v", a)
	}
}
