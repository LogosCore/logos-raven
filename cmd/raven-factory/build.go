package main

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// buildSchema is what Raven builds: PowerShell scripts, minions that check
// in through the HTTP channel (reverse-call) and do two things, hello and
// echo. Raven exists to run core's codec mechanics end to end: its
// minions write their messages as PowerShell's own CLIXML (raven.clixml)
// and its suite, raven.raven, seals nothing (codec.go). No compression:
// the envelope is Raven's, and Raven packs nothing.
var buildSchema = protocol.BuildSchema{
	SchemaVersion: protocol.BuildSchemaV3,
	Version:       1,
	Channels: []protocol.BuildChannel{{
		ModuleName: httpChannel, Num: 1, Versions: []int{3},
		Kinds: []protocol.BuildKind{{Kind: protocol.KindReverseCall, Fields: []protocol.FieldSpec{
			{
				Key: "method", Num: 1, Type: protocol.FieldEnum, Label: "Method", Default: "POST", Group: "HTTP", Order: 1,
				Description: "The HTTP method the script sends its messages with.",
				Options:     []protocol.EnumOption{{Value: "POST"}, {Value: "PUT"}},
			},
			{
				Key: "path", Num: 2, Type: protocol.FieldString, Label: "Path", Default: "/", Group: "HTTP", Order: 2,
				Description: "The path every request goes to, under the endpoint's base URL.",
				Pattern:     `^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`, MaxLength: 256,
			},
		}}},
	}},
	Crypto:   suiteName,
	Envelope: envelopeName,
	// CLIXML is verbose: every map entry is some 60 bytes of XML where CBOR
	// takes 3, so a small message grows twentyfold and a large one, mostly
	// base64, by a third. The overhead covers the small ones; core sends
	// a message again with fewer items when it outgrows the bound.
	Codec: &protocol.BuildCodec{Growth: 400, Overhead: 4096, Timeout: "5s"},
	Fields: []protocol.FieldSpec{
		{
			Key: "greeting", Num: 1, Type: protocol.FieldString, Label: "Greeting", Default: "hello from Raven", MaxLength: 200,
			Order: 1, Description: "What the script answers the hello command with.",
		},
	},
	Targets: []protocol.BuildTarget{{
		ID: targetScript, Label: "PowerShell script", Format: "script",
		Platforms:   []string{"windows/any", "linux/any", "darwin/any"},
		Description: "A .ps1 for Windows PowerShell 5.1 or PowerShell 7, which checks in through the HTTP channel. Run it with -Trace to see its exchanges.",
	}},
	Commands: []protocol.BuildCommand{
		{ID: "hello", Op: opHello, Label: "Say hello", Default: true, Description: "Answers with the build's greeting."},
		{
			ID: "echo", Op: opEcho, Label: "Echo", Default: true, Description: "Answers with its text.",
			Params: []protocol.FieldSpec{{Key: "text", Num: 1, Type: protocol.FieldString, Label: "Text", Required: true, MaxLength: 65536}},
		},
	},
}

const (
	httpChannel  = "http"
	targetScript = "ps1"
	// The commands' numbers: the script's $OpHello and $OpEcho.
	opHello = 1
	opEcho  = 2
)

//go:embed raven.ps1
var scriptTemplate string

// raven builds scripts.
type raven struct {
	// version goes into the scripts' hello (their implementation's).
	version string
}

// The steps of a build.
var (
	stepRender = protocol.BuildStep{ID: "render", Label: "Write the script"}
	stepUpload = protocol.BuildStep{ID: "upload", Label: "Upload the artifact"}
)

// Steps is every build's plan.
func (r *raven) Steps(protocol.BuildRequest) []protocol.BuildStep {
	return []protocol.BuildStep{stepRender, stepUpload}
}

func (r *raven) Build(ctx context.Context, job *factory.Job) (factory.Result, error) {
	job.Step(stepRender.ID, "")
	script, conns, err := render(job.Request, r.version)
	if err != nil {
		return factory.Result{}, err
	}
	job.Logf("a PowerShell script for build %s, %d connection(s) through the HTTP channel, %d bytes",
		job.Request.BuildID, conns, len(script))

	job.Step(stepUpload.ID, "")
	name := "raven-" + job.Request.BuildID[:8] + ".ps1"
	if err := job.UploadArtifact(ctx, name, "text/plain", strings.NewReader(script)); err != nil {
		return factory.Result{}, err
	}
	return factory.Result{}, nil
}

// render is the script a request asks for: its build id, connections,
// greeting and the commands it was built with, each value a PowerShell
// literal. The suite's keys are not in it: raven.raven seals nothing. It
// starts with UTF-8's byte order mark: Windows PowerShell 5.1 reads a
// script without one as the system's ANSI code page, which mangles any
// text past ASCII (a greeting's quotes, say).
func render(req protocol.BuildRequest, version string) (string, int, error) {
	if len(req.BuildID) != 36 {
		return "", 0, factory.Fail("bad_build_id", "the build id is not a UUID")
	}
	if req.Crypto == nil || req.Crypto.Suite != suiteName {
		return "", 0, factory.Fail("no_keys", "the build request has no "+suiteName+" keys")
	}
	conns, err := connections(req)
	if err != nil {
		return "", 0, err
	}
	greeting, _ := req.Values["greeting"].(string)
	ops := []string{}
	for _, c := range buildSchema.Commands {
		if slices.Contains(req.Commands, c.ID) {
			ops = append(ops, strconv.Itoa(c.Op))
		}
	}
	script := utf8BOM + strings.NewReplacer(
		"__BUILD_ID__", req.BuildID,
		"__VERSION__", psQuote(version),
		"__GREETING__", psQuote(greeting),
		"__OPS__", strings.Join(ops, ", "),
		"__CONNS__", strings.Join(conns, ",\n  "),
	).Replace(scriptTemplate)
	return script, len(conns), nil
}

// connections are the build's connections, each a PowerShell hashtable
// literal, in the order the script tries them.
func connections(req protocol.BuildRequest) ([]string, error) {
	var out []string
	for i, c := range req.Connections {
		if c.Endpoint.Channel.ModuleName != httpChannel || c.ConnectionKind() != protocol.KindReverseCall {
			return nil, factory.Fail("bad_connection", fmt.Sprintf("connection %d is not an HTTP reverse-call one", i+1))
		}
		p, v := c.Endpoint.Params, c.Values
		urls := []string{psQuote(str(p["base_url"]))}
		if extra, ok := p["extra_base_urls"].([]any); ok {
			for _, u := range extra {
				if s := str(u); s != "" {
					urls = append(urls, psQuote(s))
				}
			}
		}
		headers := []string{}
		if h, ok := p["headers"].(map[string]any); ok {
			names := make([]string, 0, len(h))
			for k := range h {
				names = append(names, k)
			}
			slices.Sort(names)
			for _, k := range names {
				headers = append(headers, psQuote(k)+" = "+psQuote(str(h[k])))
			}
		}
		timeout, err := millis(p["request_timeout"], 40*time.Second)
		if err != nil {
			return nil, factory.Fail("bad_connection", fmt.Sprintf("connection %d: request_timeout: %v", i+1, err))
		}
		interval, err := millis(p["checkin_interval"], time.Minute)
		if err != nil {
			return nil, factory.Fail("bad_connection", fmt.Sprintf("connection %d: checkin_interval: %v", i+1, err))
		}
		jitter := 20
		if j, ok := p["checkin_jitter"].(float64); ok {
			jitter = int(j)
		}
		verify, _ := p["tls_verify"].(bool)
		method := str(v["method"])
		if method == "" {
			method = "POST"
		}
		path := str(v["path"])
		if path == "" {
			path = "/"
		}
		out = append(out, fmt.Sprintf("@{ Place = %d; Urls = @(%s); Method = %s; Path = %s; UserAgent = %s; Headers = @{ %s }; "+
			"TimeoutMs = %d; IntervalMs = %d; Jitter = %d; TlsVerify = $%t }",
			i+1, strings.Join(urls, ", "), psQuote(method), psQuote(path), psQuote(str(p["user_agent"])),
			strings.Join(headers, "; "), timeout, interval, jitter, verify))
	}
	if len(out) == 0 {
		return nil, factory.Fail("bad_connection", "the build has no connection")
	}
	return out, nil
}

// utf8BOM marks a script as UTF-8 for Windows PowerShell 5.1.
const utf8BOM = "\ufeff"

func str(v any) string {
	s, _ := v.(string)
	return s
}

// millis reads a duration value ("40s") as milliseconds; def when absent.
func millis(v any, def time.Duration) (int64, error) {
	s := str(v)
	if s == "" {
		return def.Milliseconds(), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	return d.Milliseconds(), nil
}

// psQuote is s as a single-quoted PowerShell string: every quote doubled,
// the typographic ones PowerShell also takes for quotes too.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
