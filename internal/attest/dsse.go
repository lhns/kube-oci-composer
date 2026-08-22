package attest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// DSSE, hand-rolled, for the same reason the SPDX and in-toto structs are: the bytes must be ours
// to keep stable. It is a small, fixed specification -- a pre-authentication encoding of four
// fields, signed.

type dsseEnvelope struct {
	Payload     string          `json:"payload"`
	PayloadType string          `json:"payloadType"`
	Signatures  []dsseSignature `json:"signatures"`
}

type dsseSignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// Envelope wraps a statement in a signed DSSE envelope.
//
// This is how attestations get signed WITHOUT a second signature object: the envelope carries it.
// Pushing a separate `.sig` for the attestation manifest as well would need its own idempotence
// check for no additional guarantee.
func Envelope(key *Key, statement []byte) ([]byte, error) {
	sig, err := key.Sign(pae(MediaTypeInToto, statement))
	if err != nil {
		return nil, fmt.Errorf("signing the attestation: %w", err)
	}
	env := dsseEnvelope{
		Payload:     base64.StdEncoding.EncodeToString(statement),
		PayloadType: MediaTypeInToto,
		Signatures:  []dsseSignature{{Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
	return json.Marshal(env)
}

// pae is DSSE's Pre-Authentication Encoding.
//
// It exists so a signature over a payload cannot be replayed against a different payload TYPE --
// the lengths are part of what is signed, so no concatenation of one pair can be reinterpreted as
// another. Hence signing pae(...) rather than the payload directly.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s",
		len(payloadType), payloadType, len(payload), payload))
}
