package main

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/fxamacker/cbor/v2"
)

// Raven's envelope, raven.clixml: a message as PowerShell's own CLIXML,
// what [System.Management.Automation.PSSerializer] writes and reads, so the
// minion needs no encoder of its own. The protocol's maps are Hashtables
// (numbered keys as Int32, or Int64 past it; text keys as strings), arrays
// Object[], byte strings byte[] (BA), text strings (S), integers I32, I64
// or U64, floats Db, booleans B and null Nil. Factory side, the message
// crosses the link as CBOR (the protocol's form): clixmlToCBOR reads what
// the minion wrote, cborToCLIXML writes what core says.

// clixmlNS is CLIXML's namespace.
const clixmlNS = "http://schemas.microsoft.com/powershell/2004/04"

// maxCLIXMLDepth bounds how deep a message's values nest, one level for
// each map or array: twice the protocol's 16.
const maxCLIXMLDepth = 32

// clixmlToCBOR reads a message the minion wrote (the first object of a
// CLIXML document) as CBOR.
func clixmlToCBOR(doc []byte) ([]byte, error) {
	v, err := readCLIXML(doc)
	if err != nil {
		return nil, err
	}
	return cborEnc.Marshal(v)
}

// readCLIXML reads the first object of a CLIXML document.
func readCLIXML(doc []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(doc))
	d.Strict = true
	p := &clixmlReader{d: d, refs: map[string]any{}}
	root, err := p.next()
	if err != nil {
		return nil, err
	}
	if root.Name.Local != "Objs" {
		return nil, fmt.Errorf("clixml: the root is <%s>, not <Objs>", root.Name.Local)
	}
	first, err := p.next()
	if err != nil {
		return nil, fmt.Errorf("clixml: no object: %w", err)
	}
	return p.value(first, 0)
}

type clixmlReader struct {
	d    *xml.Decoder
	refs map[string]any
}

// errEnd is the end of the element whose children are read.
var errEnd = errors.New("end of element")

// next is the next child element's start, skipping text and comments;
// errEnd at the end of the parent.
func (p *clixmlReader) next() (xml.StartElement, error) {
	for {
		tok, err := p.d.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.EndElement:
			return xml.StartElement{}, errEnd
		}
	}
}

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// text is the character data of the element started, up to its end.
func (p *clixmlReader) text() (string, error) {
	var b strings.Builder
	for {
		tok, err := p.d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			return b.String(), nil
		case xml.StartElement:
			return "", fmt.Errorf("clixml: <%s> inside a value", t.Name.Local)
		}
	}
}

// value reads the element se started as a Go value CBOR writes: maps as
// map[any]any, arrays as []any, integers as int64 or uint64.
func (p *clixmlReader) value(se xml.StartElement, depth int) (any, error) {
	if depth > maxCLIXMLDepth {
		return nil, errors.New("clixml: nested too deep")
	}
	switch se.Name.Local {
	case "Obj":
		return p.object(se, depth)
	case "Ref":
		v, ok := p.refs[attr(se, "RefId")]
		if !ok {
			return nil, fmt.Errorf("clixml: a reference to no object (%q)", attr(se, "RefId"))
		}
		return v, p.d.Skip()
	case "Nil":
		return nil, p.d.Skip()
	}
	s, err := p.text()
	if err != nil {
		return nil, err
	}
	return primitive(se.Name.Local, s)
}

// primitive is a primitive element's value.
func primitive(tag, s string) (any, error) {
	bad := func(err error) (any, error) { return nil, fmt.Errorf("clixml: <%s>%q: %v", tag, s, err) }
	switch tag {
	case "S", "DT", "TS", "G", "URI", "Version", "XD", "SBK":
		return unescapeCLIXML(s), nil
	case "B":
		switch strings.TrimSpace(s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return bad(errors.New("not a boolean"))
	case "SB", "I16", "I32", "I64":
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return bad(err)
		}
		if n >= 0 {
			return uint64(n), nil
		}
		return n, nil
	case "By", "U16", "U32", "U64":
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return bad(err)
		}
		return n, nil
	case "Db", "Sg", "D":
		switch t := strings.TrimSpace(s); t {
		case "INF", "Infinity":
			return math.Inf(1), nil
		case "-INF", "-Infinity":
			return math.Inf(-1), nil
		case "NaN":
			return math.NaN(), nil
		default:
			f, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return bad(err)
			}
			return f, nil
		}
	case "C":
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
		if err != nil {
			return bad(err)
		}
		return string(rune(n)), nil
	case "BA":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil {
			return bad(err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("clixml: a <%s> Raven does not read", tag)
}

// object reads an Obj: a dictionary (DCT) as a map, a list (LST, IE, STK,
// QUE) as an array, an object's members (MS, Props) as a map with text
// keys, else its ToString.
func (p *clixmlReader) object(se xml.StartElement, depth int) (any, error) {
	var out any
	var members map[any]any
	var str *string
	for {
		c, err := p.next()
		if errors.Is(err, errEnd) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch c.Name.Local {
		case "TN", "TNRef":
			if err := p.d.Skip(); err != nil {
				return nil, err
			}
		case "ToString":
			s, err := p.text()
			if err != nil {
				return nil, err
			}
			s = unescapeCLIXML(s)
			str = &s
		case "DCT":
			m, err := p.dictionary(depth)
			if err != nil {
				return nil, err
			}
			out = m
		case "LST", "IE", "STK", "QUE":
			l, err := p.list(depth)
			if err != nil {
				return nil, err
			}
			out = l
		case "MS", "Props":
			if members == nil {
				members = map[any]any{}
			}
			for {
				m, err := p.next()
				if errors.Is(err, errEnd) {
					break
				}
				if err != nil {
					return nil, err
				}
				v, err := p.value(m, depth+1)
				if err != nil {
					return nil, err
				}
				members[unescapeCLIXML(attr(m, "N"))] = v
			}
		default:
			if err := p.d.Skip(); err != nil {
				return nil, err
			}
		}
	}
	switch {
	case out != nil:
	case members != nil:
		out = members
	case str != nil:
		out = *str
	}
	if id := attr(se, "RefId"); id != "" {
		p.refs[id] = out
	}
	return out, nil
}

// dictionary reads a DCT's En entries, each a Key and a Value.
func (p *clixmlReader) dictionary(depth int) (map[any]any, error) {
	m := map[any]any{}
	for {
		en, err := p.next()
		if errors.Is(err, errEnd) {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		if en.Name.Local != "En" {
			return nil, fmt.Errorf("clixml: <%s> in a dictionary", en.Name.Local)
		}
		var key, val any
		var hasKey bool
		for {
			kv, err := p.next()
			if errors.Is(err, errEnd) {
				break
			}
			if err != nil {
				return nil, err
			}
			v, err := p.value(kv, depth+1)
			if err != nil {
				return nil, err
			}
			switch attr(kv, "N") {
			case "Key":
				key, hasKey = v, true
			case "Value":
				val = v
			}
		}
		switch key.(type) {
		case uint64, int64, string:
		default:
			if hasKey {
				return nil, fmt.Errorf("clixml: a dictionary key of %T", key)
			}
			return nil, errors.New("clixml: an entry without its key")
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("clixml: the key %v twice", key)
		}
		m[key] = val
	}
}

// list reads a list's items.
func (p *clixmlReader) list(depth int) ([]any, error) {
	l := []any{}
	for {
		it, err := p.next()
		if errors.Is(err, errEnd) {
			return l, nil
		}
		if err != nil {
			return nil, err
		}
		v, err := p.value(it, depth+1)
		if err != nil {
			return nil, err
		}
		l = append(l, v)
	}
}

// unescapeCLIXML undoes CLIXML's escapes in a string: _xHHHH_ is the UTF-16
// unit HHHH (a pair of them a character past U+FFFF).
func unescapeCLIXML(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	var units []uint16
	var b strings.Builder
	flush := func() {
		if len(units) > 0 {
			b.WriteString(string(utf16.Decode(units)))
			units = units[:0]
		}
	}
	for i := 0; i < len(s); {
		if i+7 <= len(s) && s[i] == '_' && s[i+1] == 'x' && s[i+6] == '_' {
			if n, err := strconv.ParseUint(s[i+2:i+6], 16, 16); err == nil {
				units = append(units, uint16(n))
				i += 7
				continue
			}
		}
		flush()
		b.WriteByte(s[i])
		i++
	}
	flush()
	return b.String()
}

// escapeCLIXML escapes s as PowerShell does: control characters, the
// characters XML cannot hold, and a "_" before "x" as _xHHHH_; then XML's
// own.
func escapeCLIXML(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0xd800 && r <= 0xdfff) || r == 0xfffe || r == 0xffff,
			r == '_' && i+1 < len(rs) && rs[i+1] == 'x':
			fmt.Fprintf(&b, "_x%04X_", r)
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var (
	cborEnc  cbor.EncMode
	cborTree cbor.DecMode
)

func init() {
	var err error
	if cborEnc, err = (cbor.EncOptions{Sort: cbor.SortCoreDeterministic, ShortestFloat: cbor.ShortestFloat16,
		IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden}).EncMode(); err != nil {
		panic(err)
	}
	if cborTree, err = (cbor.DecOptions{DefaultMapType: reflect.TypeFor[map[any]any](), MaxNestedLevels: 32,
		IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden}).DecMode(); err != nil {
		panic(err)
	}
}

// cborToCLIXML writes a message in CBOR (what core says) as CLIXML, which
// PSSerializer.Deserialize reads.
func cborToCLIXML(msg []byte) ([]byte, error) {
	var v any
	if err := cborTree.Unmarshal(msg, &v); err != nil {
		return nil, fmt.Errorf("cbor: %w", err)
	}
	w := &clixmlWriter{tn: map[string]int{}}
	w.b.WriteString(`<Objs Version="1.1.0.1" xmlns="` + clixmlNS + `">`)
	if err := w.value(v, ""); err != nil {
		return nil, err
	}
	w.b.WriteString("</Objs>")
	return w.b.Bytes(), nil
}

type clixmlWriter struct {
	b    bytes.Buffer
	refs int
	// tn are the type names written, by their RefId: the next of a kind is
	// a TNRef.
	tn map[string]int
}

// Type names: a Hashtable's and an Object[]'s.
var (
	hashtableTN = []string{"System.Collections.Hashtable", "System.Object"}
	arrayTN     = []string{"System.Object[]", "System.Array", "System.Object"}
)

func nameAttr(n string) string {
	if n == "" {
		return ""
	}
	return ` N="` + n + `"`
}

// open writes an Obj's start with its type names.
func (w *clixmlWriter) open(n string, tn []string) {
	fmt.Fprintf(&w.b, `<Obj%s RefId="%d">`, nameAttr(n), w.refs)
	w.refs++
	if id, ok := w.tn[tn[0]]; ok {
		fmt.Fprintf(&w.b, `<TNRef RefId="%d" />`, id)
		return
	}
	id := len(w.tn)
	w.tn[tn[0]] = id
	fmt.Fprintf(&w.b, `<TN RefId="%d">`, id)
	for _, t := range tn {
		w.b.WriteString("<T>" + t + "</T>")
	}
	w.b.WriteString("</TN>")
}

// value writes v as an element named n ("" for none).
func (w *clixmlWriter) value(v any, n string) error {
	na := nameAttr(n)
	switch v := v.(type) {
	case nil:
		w.b.WriteString("<Nil" + na + " />")
	case bool:
		fmt.Fprintf(&w.b, "<B%s>%t</B>", na, v)
	case uint64:
		switch {
		case v <= math.MaxInt32:
			fmt.Fprintf(&w.b, "<I32%s>%d</I32>", na, v)
		case v <= math.MaxInt64:
			fmt.Fprintf(&w.b, "<I64%s>%d</I64>", na, v)
		default:
			fmt.Fprintf(&w.b, "<U64%s>%d</U64>", na, v)
		}
	case int64:
		if v >= math.MinInt32 {
			fmt.Fprintf(&w.b, "<I32%s>%d</I32>", na, v)
		} else {
			fmt.Fprintf(&w.b, "<I64%s>%d</I64>", na, v)
		}
	case float64:
		s := strconv.FormatFloat(v, 'g', -1, 64)
		switch {
		case math.IsNaN(v):
			s = "NaN"
		case math.IsInf(v, 1):
			s = "INF"
		case math.IsInf(v, -1):
			s = "-INF"
		}
		fmt.Fprintf(&w.b, "<Db%s>%s</Db>", na, s)
	case string:
		fmt.Fprintf(&w.b, "<S%s>%s</S>", na, escapeCLIXML(v))
	case []byte:
		fmt.Fprintf(&w.b, "<BA%s>%s</BA>", na, base64.StdEncoding.EncodeToString(v))
	case []any:
		w.open(n, arrayTN)
		w.b.WriteString("<LST>")
		for _, it := range v {
			if err := w.value(it, ""); err != nil {
				return err
			}
		}
		w.b.WriteString("</LST></Obj>")
	case map[any]any:
		w.open(n, hashtableTN)
		w.b.WriteString("<DCT>")
		keys, err := sortedKeys(v)
		if err != nil {
			return err
		}
		for _, k := range keys {
			w.b.WriteString("<En>")
			if err := w.value(k, "Key"); err != nil {
				return err
			}
			if err := w.value(v[k], "Value"); err != nil {
				return err
			}
			w.b.WriteString("</En>")
		}
		w.b.WriteString("</DCT></Obj>")
	default:
		return fmt.Errorf("clixml: a %T in core's message", v)
	}
	return nil
}

// sortedKeys are m's keys, numbers first in order, then text.
func sortedKeys(m map[any]any) ([]any, error) {
	keys := make([]any, 0, len(m))
	for k := range m {
		switch k.(type) {
		case uint64, int64, string:
		default:
			return nil, fmt.Errorf("clixml: a map key of %T", k)
		}
		keys = append(keys, k)
	}
	num := func(k any) (float64, bool) {
		switch k := k.(type) {
		case uint64:
			return float64(k), true
		case int64:
			return float64(k), true
		}
		return 0, false
	}
	slices.SortFunc(keys, func(a, b any) int {
		na, aok := num(a)
		nb, bok := num(b)
		switch {
		case aok && bok:
			return int(math.Copysign(1, na-nb)) * btoi(na != nb)
		case aok:
			return -1
		case bok:
			return 1
		}
		return strings.Compare(a.(string), b.(string))
	})
	return keys, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
