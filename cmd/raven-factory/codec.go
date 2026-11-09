package main

import (
	"context"
	"crypto/rand"

	"github.com/logoscore/logos-golang-factory-core/pkg/codeclink"
)

// Raven's codec, which it translates for core over the factory link
// (factory-core's codeclink): its envelope, raven.clixml (clixml.go), and
// its suite, raven.raven.
//
// raven.raven is a lie told to core: it seals nothing. Core sees a suite of
// the factory's own and hands Raven its keys with every message; Raven
// makes them for each build and never uses them, and its minions send
// their CLIXML in the clear. It exists to run the codec mechanics end to
// end, never to protect a message.
const (
	suiteName    = "raven.raven"
	envelopeName = "raven.clixml"
)

type ravenCodec struct{}

// Inbound reads a minion's message: the suite opens nothing (the first key
// "opens" it), the envelope reads its CLIXML as CBOR.
func (ravenCodec) Inbound(_ context.Context, in codeclink.Inbound) (codeclink.Opened, error) {
	data := in.Data
	if in.Envelope {
		b, err := clixmlToCBOR(data)
		if err != nil {
			return codeclink.Opened{}, codeclink.NotMessage("%v", err)
		}
		data = b
	}
	return codeclink.Opened{Data: data, Key: 0}, nil
}

// Outbound writes core's message as CLIXML; the suite seals nothing.
func (ravenCodec) Outbound(_ context.Context, out codeclink.Outbound) ([]byte, error) {
	if !out.Envelope {
		return out.Data, nil
	}
	return cborToCLIXML(out.Data)
}

// Keys are a build's: 32 random bytes nobody uses. A stage is refused: a
// Raven minion says hello under its build id.
func (ravenCodec) Keys(_ context.Context, k codeclink.KeysFor) (codeclink.Keys, error) {
	if k.Stage {
		return codeclink.Keys{}, codeclink.Refused("%s does not stage: Raven's minions say hello under their build id", suiteName)
	}
	m := make([]byte, 32)
	_, _ = rand.Read(m)
	return codeclink.Keys{Material: m}, nil
}
