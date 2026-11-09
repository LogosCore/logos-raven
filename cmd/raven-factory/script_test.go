package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/logoscore/logos-golang-factory-core/pkg/codeclink"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The rendered script under PowerShell, spoken to as the HTTP channel and
// core speak to it: the test server unpacks the channel's body, reads the
// CLIXML with Raven's codec as core would have it read (the factory link's
// inbound call, both layers), and answers with core's message written by
// the codec (outbound). pwsh runs it: RAVEN_PWSH names the command (a
// wrapper running PowerShell's image, say), else pwsh on PATH; without
// either the tests skip, unless RAVEN_REQUIRE_PWSH is set (CI).

func pwsh(t *testing.T) []string {
	t.Helper()
	if c := os.Getenv("RAVEN_PWSH"); c != "" {
		return strings.Fields(c)
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return []string{p}
	}
	if os.Getenv("RAVEN_REQUIRE_PWSH") != "" {
		t.Fatal("no pwsh: set RAVEN_PWSH or put pwsh on PATH")
	}
	t.Skip("no pwsh (RAVEN_PWSH, or pwsh on PATH)")
	return nil
}

// fakeCore is the HTTP channel and core in one: each request's message,
// read as core reads it, goes to answer, whose items go back.
type fakeCore struct {
	t      *testing.T
	build  uuid.UUID
	key    []byte
	mu     sync.Mutex
	answer func(r *http.Request, under uuid.UUID, msg protocol.MinionMessage) []any
	seen   int
}

func (c *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if len(body) < 17 || int(body[0])+17 > len(body) {
		http.NotFound(w, r)
		return
	}
	at := int(body[0])
	var under uuid.UUID
	copy(under[:], body[1+at:17+at])
	payload := append(append([]byte{}, body[1:1+at]...), body[17+at:]...)
	ctx := context.Background()
	in := codeclink.Answer(ctx, ravenCodec{}, protocol.FactoryCall{Type: protocol.FactoryInbound, Request: 1, Build: c.build[:],
		ID: under[:], Data: payload, Deadline: 5000, Layers: []int{protocol.CodecLayerSuite, protocol.CodecLayerEnvelope},
		Keys: [][]byte{c.key}})
	if in.Failure != nil {
		c.t.Errorf("the minion's message does not read: %s\n%s", in.Failure.Msg, payload)
		http.NotFound(w, r)
		return
	}
	var msg protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(in.Data, &msg); err != nil || protocol.ValidateMessageHead(msg.V, msg.ID, msg.Re, len(msg.Items)) != nil {
		c.t.Errorf("not a minion message: %v", err)
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	c.seen++
	items := c.answer(r, under, msg)
	c.mu.Unlock()
	if len(items) == 1 && items[0] == notFound {
		// The channel's answer to any failure.
		http.NotFound(w, r)
		return
	}
	reply := []byte{}
	if len(items) > 0 {
		raws := make([]cbor.RawMessage, len(items))
		for i, it := range items {
			raws[i], _ = protocol.MarshalCBOR(it)
		}
		cm, _ := protocol.MarshalCBOR(protocol.CoreMessage{V: protocol.MinionMessageV3, Re: msg.ID, Items: raws})
		out := codeclink.Answer(ctx, ravenCodec{}, protocol.FactoryCall{Type: protocol.FactoryOutbound, Request: 1, Build: c.build[:],
			ID: under[:], Data: cm, Deadline: 5000, Layers: []int{protocol.CodecLayerSuite, protocol.CodecLayerEnvelope},
			Keys: [][]byte{c.key}})
		if out.Failure != nil {
			c.t.Errorf("core's message does not write: %s", out.Failure.Msg)
		}
		reply = out.Data
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(min(256, len(reply)+1))))
	off := int(n.Int64())
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(append(append(append([]byte{byte(off)}, reply[:off]...), under[:]...), reply[off:]...))
}

// script renders a script for c's build with connections conns (params,
// values) and writes it to a directory PowerShell's container sees.
func (c *fakeCore) script(t *testing.T, conns ...[2]map[string]any) string {
	t.Helper()
	material, _ := json.Marshal(c.key)
	req := protocol.BuildRequest{BuildID: c.build.String(), Target: targetScript, Platform: "linux/any",
		Values:   map[string]any{"greeting": "hello from Raven's ‘test’, it's"},
		Commands: []string{"hello", "echo"}, Crypto: &protocol.BuildCrypto{Suite: suiteName, Material: material}}
	for _, cv := range conns {
		req.Connections = append(req.Connections, protocol.BuildConnection{
			Endpoint: protocol.ChannelEndpointExport{Format: protocol.ChannelEndpointExportV2, Name: "edge",
				Channel: protocol.ExportChannel{ModuleName: httpChannel, SchemaVersion: 3}, Kind: protocol.KindReverseCall, Params: cv[0]},
			Values: cv[1]})
	}
	s, _, err := render(req, "test")
	if err != nil {
		t.Fatal(err)
	}
	// The system's temporary folder, not t.TempDir: a container running
	// PowerShell mounts it (/tmp).
	dir, err := os.MkdirTemp("", "raven-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "raven.ps1")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// run runs the script until done is closed, for at most seconds; with
// forever, without -Seconds (as an operator runs it), stopped by the test.
func run(t *testing.T, script string, seconds int, forever bool, done <-chan struct{}) string {
	t.Helper()
	cmd := pwsh(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+30)*time.Second)
	defer cancel()
	args := append(cmd[1:], "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-Trace")
	if !forever {
		args = append(args, "-Seconds", strconv.Itoa(seconds))
	}
	c := exec.CommandContext(ctx, cmd[0], args...)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	select {
	case <-done:
		if c.Process.Signal(os.Interrupt) != nil {
			_ = c.Process.Kill() // Windows: no interrupt to send
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = c.Process.Kill()
		}
	case err := <-exited:
		if err != nil {
			t.Logf("pwsh: %v", err)
		}
	}
	return out.String()
}

// notFound, as answer's one item, answers 404: the exchange failed.
const notFound = "404"

func taskItem(id uuid.UUID, op uint64, args protocol.Args) protocol.TaskItem {
	return protocol.TaskItem{T: protocol.ItemTask, Task: id[:], Cmd: op, Args: args}
}

// A whole life: hello under the build id (through the second of its URLs,
// the first refusing), tasks run and their results resent until taken, a
// rehello, and a closed session started over.
func TestScript(t *testing.T) {
	core := &fakeCore{t: t, build: uuid.New(), key: []byte("raven key, never used")}
	srv := httptest.NewServer(core)
	defer srv.Close()
	sid, sid2 := uuid.New(), uuid.New()
	tHello, tEcho, tOther := uuid.New(), uuid.New(), uuid.New()
	echoText := "a\n<b>&\"c\" _x000A_ ✓ 🐦 'q' ‘q’"
	done := make(chan struct{})
	var once sync.Once
	step := 0
	// fail reports from the server's goroutine and stops the run.
	fail := func(format string, args ...any) []any {
		t.Errorf(format, args...)
		once.Do(func() { close(done) })
		return nil
	}
	core.answer = func(r *http.Request, under uuid.UUID, msg protocol.MinionMessage) []any {
		if r.Method != http.MethodPut || r.URL.Path != "/r/" || r.Header.Get("X-Raven") != "yes" || r.UserAgent() != "Raven/1.0" {
			t.Errorf("request %s %s, X-Raven %q, User-Agent %q", r.Method, r.URL.Path, r.Header.Get("X-Raven"), r.UserAgent())
		}
		types := []int{}
		for _, it := range msg.Items {
			types = append(types, protocol.ItemType(it))
		}
		step++
		switch step {
		case 1:
			// The first message: one hello, under the build id.
			if under != core.build || len(msg.Items) != 1 || types[0] != protocol.ItemHello {
				return fail("first message under %s: %v", under, types)
			}
			var h protocol.HelloItem
			if err := protocol.UnmarshalCBOR(msg.Items[0], &h); err != nil || h.Validate() != nil {
				return fail("hello %+v: %v %v", h, err, h.Validate())
			}
			source := map[string][]int{"linux": {protocol.MachineLinuxMachineID, protocol.MachineHostname},
				"windows": {protocol.MachineWinMachineGUID}}[runtime.GOOS]
			if h.OS != runtime.GOOS || h.Arch == "" || h.PID == 0 || !slices.Contains(source, h.MachineSource) ||
				h.Sleep == nil || h.Sleep.Interval != 300 || h.Sleep.Jitter != 0 || h.Conn != 1 || h.Version != "raven test" || h.X["powershell"] == nil {
				t.Errorf("hello %+v", h)
			}
			return []any{protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: sid[:]}}
		case 2:
			if under != sid || types[0] != protocol.ItemGetTasks {
				return fail("second message under %s: %v", under, types)
			}
			return []any{taskItem(tHello, opHello, nil), taskItem(tEcho, opEcho, protocol.Args{1: echoText}),
				taskItem(tOther, 99, nil)}
		case 3:
			// Refused (a 404): its acks and results must come again.
			if len(msg.Items) != 7 {
				t.Errorf("third message: %v", types)
			}
			return []any{notFound}
		case 4:
			results := map[uuid.UUID]protocol.ResultItem{}
			acks := 0
			for _, raw := range msg.Items {
				switch protocol.ItemType(raw) {
				case protocol.ItemAck:
					acks++
				case protocol.ItemResult:
					var res protocol.ResultItem
					if err := protocol.UnmarshalCBOR(raw, &res); err != nil || res.Validate() != nil {
						return fail("result: %v", err)
					}
					results[uuid.UUID(res.Task)] = res
				}
			}
			if acks != 3 || len(results) != 3 {
				return fail("again: %d acks, %d results (%v)", acks, len(results), types)
			}
			if r := results[tHello]; !*r.OK || string(r.Out) != "hello from Raven's ‘test’, it's" {
				t.Errorf("hello: %+v %q", r, r.Out)
			}
			if r := results[tEcho]; !*r.OK || string(r.Out) != echoText {
				t.Errorf("echo: %q, want %q", r.Out, echoText)
			}
			if r := results[tOther]; *r.OK || !strings.Contains(r.Err, "command 99") {
				t.Errorf("an unknown command: %+v", r)
			}
			// Core's empty reply (its 204): it took them, and had nothing.
			return nil
		case 5:
			// Taken: only the check-in.
			if under != sid || len(msg.Items) != 1 || types[0] != protocol.ItemGetTasks {
				t.Errorf("after core took them: %v", types)
			}
			return []any{protocol.RehelloItem{T: protocol.ItemRehello}, protocol.SleepUpdateItem{T: protocol.ItemSleepUpdate, Interval: 200}}
		case 6:
			// The rehello's hello, with the schedule core set.
			var h protocol.HelloItem
			for _, raw := range msg.Items {
				if protocol.ItemType(raw) == protocol.ItemHello {
					_ = protocol.UnmarshalCBOR(raw, &h)
				}
			}
			if under != sid || h.Sleep == nil || h.Sleep.Interval != 200 {
				t.Errorf("rehello under %s: %+v (%v)", under, h, types)
			}
			one := 0
			return []any{protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "closed", Item: &one}}
		case 7:
			if under != core.build || types[0] != protocol.ItemHello {
				t.Errorf("after the session closed, under %s: %v", under, types)
			}
			once.Do(func() { close(done) })
			return []any{protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: sid2[:]}}
		}
		return nil
	}
	params := map[string]any{"base_url": "http://127.0.0.1:1", "extra_base_urls": []any{srv.URL}, "user_agent": "Raven/1.0",
		"headers": map[string]any{"X-Raven": "yes"}, "request_timeout": "5s", "checkin_interval": "300ms", "checkin_jitter": float64(0)}
	script := core.script(t, [2]map[string]any{params, {"method": "PUT", "path": "/r/"}})
	out := run(t, script, 40, true, done)
	select {
	case <-done:
	default:
		t.Fatalf("the script stopped at step %d:\n%s", step, out)
	}
	if !strings.Contains(out, "exchange failed") && !strings.Contains(out, "session ") {
		t.Logf("trace:\n%s", out)
	}
}

// Over HTTPS with a certificate nobody trusts, a connection that does not
// verify TLS speaks all the same.
func TestScriptTLS(t *testing.T) {
	core := &fakeCore{t: t, build: uuid.New(), key: []byte("k")}
	srv := httptest.NewTLSServer(core)
	defer srv.Close()
	done := make(chan struct{})
	var once sync.Once
	core.answer = func(_ *http.Request, under uuid.UUID, msg protocol.MinionMessage) []any {
		if under == core.build && protocol.ItemType(msg.Items[0]) == protocol.ItemHello {
			once.Do(func() { close(done) })
		}
		return nil
	}
	params := map[string]any{"base_url": srv.URL, "tls_verify": false, "checkin_interval": "500ms", "checkin_jitter": float64(0)}
	out := run(t, core.script(t, [2]map[string]any{params, {}}), 30, false, done)
	select {
	case <-done:
	default:
		t.Fatalf("no hello over HTTPS:\n%s", out)
	}
}
