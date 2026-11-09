package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// psWritten is what PowerShell 7.4's PSSerializer wrote for
// @{0=3; 1=[byte[]](1..16); 3=[object[]]@(@{0=8; 1=[byte[]](1,2,3);
// 2=[int64]5; 3=@{1="a`nb _x"}}, @{0=7; 1=4}); 4=$true; 5=$null; 6=2.5;
// 7=[object[]]@()}, as it indents it.
const psWritten = `<Objs Version="1.1.0.1" xmlns="http://schemas.microsoft.com/powershell/2004/04">
  <Obj RefId="0">
    <TN RefId="0">
      <T>System.Collections.Hashtable</T>
      <T>System.Object</T>
    </TN>
    <DCT>
      <En><I32 N="Key">7</I32><Obj N="Value" RefId="1"><TN RefId="1"><T>System.Object[]</T><T>System.Array</T><T>System.Object</T></TN><LST /></Obj></En>
      <En><I32 N="Key">6</I32><Db N="Value">2.5</Db></En>
      <En><I32 N="Key">5</I32><Nil N="Value" /></En>
      <En><I32 N="Key">4</I32><B N="Value">true</B></En>
      <En>
        <I32 N="Key">3</I32>
        <Obj N="Value" RefId="2">
          <TNRef RefId="1" />
          <LST>
            <Obj RefId="3">
              <TNRef RefId="0" />
              <DCT>
                <En><I32 N="Key">3</I32><Obj N="Value" RefId="4"><TNRef RefId="0" /><DCT><En><I32 N="Key">1</I32><S N="Value">a_x000A_b _x005F_x</S></En></DCT></Obj></En>
                <En><I32 N="Key">2</I32><I64 N="Value">5</I64></En>
                <En><I32 N="Key">1</I32><BA N="Value">AQID</BA></En>
                <En><I32 N="Key">0</I32><I32 N="Value">8</I32></En>
              </DCT>
            </Obj>
            <Obj RefId="5"><TNRef RefId="0" /><DCT><En><I32 N="Key">1</I32><I32 N="Value">4</I32></En><En><I32 N="Key">0</I32><I32 N="Value">7</I32></En></DCT></Obj>
          </LST>
        </Obj>
      </En>
      <En><I32 N="Key">1</I32><BA N="Value">AQIDBAUGBwgJCgsMDQ4PEA==</BA></En>
      <En><I32 N="Key">0</I32><I32 N="Value">3</I32></En>
    </DCT>
  </Obj>
</Objs>`

func TestReadPowerShellCLIXML(t *testing.T) {
	v, err := readCLIXML([]byte(psWritten))
	if err != nil {
		t.Fatal(err)
	}
	want := map[any]any{
		uint64(0): uint64(3),
		uint64(1): []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		uint64(3): []any{
			map[any]any{uint64(0): uint64(8), uint64(1): []byte{1, 2, 3}, uint64(2): uint64(5), uint64(3): map[any]any{uint64(1): "a\nb _x"}},
			map[any]any{uint64(0): uint64(7), uint64(1): uint64(4)},
		},
		uint64(4): true, uint64(5): nil, uint64(6): 2.5, uint64(7): []any{},
	}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("read\n%#v\nwant\n%#v", v, want)
	}
	// As core reads it: a minion message, its items the protocol's.
	b, err := clixmlToCBOR([]byte(psWritten))
	if err != nil {
		t.Fatal(err)
	}
	var m protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(b, &m); err != nil || m.V != 3 || len(m.ID) != 16 || len(m.Items) != 2 ||
		protocol.ItemType(m.Items[1]) != protocol.ItemGetTasks {
		t.Fatalf("message %+v: %v", m, err)
	}
	var g protocol.GetTasksItem
	if err := protocol.UnmarshalCBOR(m.Items[1], &g); err != nil || g.Max == nil || *g.Max != 4 {
		t.Fatalf("get_tasks %+v: %v", g, err)
	}
}

func mustCBOR(t *testing.T, v any) cbor.RawMessage {
	t.Helper()
	b, err := protocol.MarshalCBOR(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every item core may send a Raven minion, and every one it reads, goes
// to CLIXML and back as it was.
func TestCLIXMLRoundTrip(t *testing.T) {
	task := bytes.Repeat([]byte{7}, 16)
	ok, exit, one, five := true, int64(-3), 1, uint64(5)
	items := []any{
		protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: task},
		protocol.TaskItem{T: protocol.ItemTask, Task: task, Cmd: opEcho, Args: protocol.Args{1: "a\n<b>&\"c\" _x _x000A_ \x01 ✓ 🐦"}},
		protocol.SleepUpdateItem{T: protocol.ItemSleepUpdate, Interval: 5000, Jitter: 10},
		protocol.RehelloItem{T: protocol.ItemRehello},
		protocol.PollItem{T: protocol.ItemPoll, Within: 1000},
		protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "closed", Item: &one},
		protocol.CancelItem{T: protocol.ItemCancel, Task: task},
		protocol.HelloItem{T: protocol.ItemHello, PID: 1 << 40, OS: "windows", Arch: "amd64", Host: "WS01", User: `CORP\svc`,
			MachineSource: protocol.MachineWinMachineGUID, MachineValue: "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f", Version: "raven dev",
			X: map[string]any{"powershell": "5.1.19041.1", "n": uint64(3)}, Sleep: &protocol.Sleep{Interval: 60000, Jitter: 20}, Conn: 1},
		protocol.GetTasksItem{T: protocol.ItemGetTasks, Max: &five},
		protocol.AckItem{T: protocol.ItemAck, Tasks: [][]byte{task, task}},
		protocol.ResultItem{T: protocol.ItemResult, Task: task, Seq: 0, Done: true, OK: &ok, Out: []byte("hello\x00\xff"), Exit: &exit},
	}
	raws := make([]cbor.RawMessage, len(items))
	for i, it := range items {
		raws[i] = mustCBOR(t, it)
	}
	msg := mustCBOR(t, protocol.CoreMessage{V: protocol.MinionMessageV3, ID: protocol.NewMessageID(), Items: raws, At: 1_762_700_000})
	x, err := cborToCLIXML(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(x, []byte(`<Objs Version="1.1.0.1" xmlns="`+clixmlNS+`"><Obj RefId="0"><TN RefId="0"><T>System.Collections.Hashtable</T>`)) {
		t.Fatalf("not CLIXML: %.200s", x)
	}
	back, err := clixmlToCBOR(x)
	if err != nil {
		t.Fatalf("%v\n%s", err, x)
	}
	var m protocol.CoreMessage
	if err := protocol.UnmarshalCBOR(back, &m); err != nil || len(m.Items) != len(items) || m.At != 1_762_700_000 {
		t.Fatalf("message %+v: %v", m, err)
	}
	for i, it := range items {
		got := reflect.New(reflect.TypeOf(it))
		if err := protocol.UnmarshalCBOR(m.Items[i], got.Interface()); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
		if !reflect.DeepEqual(got.Elem().Interface(), it) {
			t.Fatalf("item %d:\n got %#v\nwant %#v", i, got.Elem().Interface(), it)
		}
	}
	for _, want := range []string{`a_x000A_&lt;b&gt;&amp;"c" _x005F_x _x005F_x000A_ _x0001_ ✓ 🐦`, `<I64 N="Value">1099511627776</I64>`,
		`<I32 N="Value">-3</I32>`, `<S N="Key">powershell</S>`} {
		if !strings.Contains(string(x), want) {
			t.Errorf("CLIXML lacks %s", want)
		}
	}
}

func TestUnescapeCLIXML(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                "plain",
		"a_x000A_b":            "a\nb",
		"_x005F_x":             "_x",
		"_xD83D__xDC26_":       "🐦",
		"_x00_ _xZZZZ_ _x0041": "_x00_ _xZZZZ_ _x0041",
	} {
		if got := unescapeCLIXML(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
		if in == "plain" || strings.Contains(in, "ZZ") {
			continue
		}
		if got := unescapeCLIXML(escapeCLIXML(want)); got != want {
			t.Errorf("%q does not go back: %q", want, got)
		}
	}
}

func TestReadCLIXMLRefusals(t *testing.T) {
	deep := strings.Repeat(`<Obj><LST>`, 50) + strings.Repeat(`</LST></Obj>`, 50)
	for name, doc := range map[string]string{
		"not xml":        `<Objs><Obj>`,
		"another root":   `<Obj RefId="0"><DCT/></Obj>`,
		"no object":      `<Objs Version="1.1.0.1"></Objs>`,
		"a key twice":    `<Objs><Obj><DCT><En><I32 N="Key">0</I32><I32 N="Value">1</I32></En><En><I32 N="Key">0</I32><I32 N="Value">2</I32></En></DCT></Obj></Objs>`,
		"no key":         `<Objs><Obj><DCT><En><I32 N="Value">1</I32></En></DCT></Obj></Objs>`,
		"a list key":     `<Objs><Obj><DCT><En><Obj N="Key"><LST/></Obj><I32 N="Value">1</I32></En></DCT></Obj></Objs>`,
		"bad base64":     `<Objs><BA>!!</BA></Objs>`,
		"bad integer":    `<Objs><I32>x</I32></Objs>`,
		"unknown tag":    `<Objs><Blob>x</Blob></Objs>`,
		"a dangling ref": `<Objs><Ref RefId="9" /></Objs>`,
		"too deep":       `<Objs>` + deep + `</Objs>`,
	} {
		if _, err := readCLIXML([]byte(doc)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// A reference reads as the object it names.
	v, err := readCLIXML([]byte(`<Objs><Obj><LST><Obj RefId="7"><DCT><En><S N="Key">a</S><I32 N="Value">1</I32></En></DCT></Obj><Ref RefId="7" /></LST></Obj></Objs>`))
	if err != nil || !reflect.DeepEqual(v, []any{map[any]any{"a": uint64(1)}, map[any]any{"a": uint64(1)}}) {
		t.Fatalf("a reference: %#v %v", v, err)
	}
}
