package attest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// DSSE, hand-rolled like the SPDX and in-toto structs so the bytes stay ours to keep stable.

type dsseEnvelope struct {
	Payload     string          `json:"payload"`
	PayloadType string          `json:"payloadType"`
	Signatures  []dsseSignature `json:"signatures"`
}

type dsseSignature struct {
	// KeyID is always empty but serialised: dropping it or adding omitempty changes the envelope
	// bytes and so every attestation's digest under an unchanged spec (ADR 0057). The signature
	// covers the PAE-encoded payload, not this JSON.
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// Envelope wraps a statement in a signed DSSE envelope, so an attestation needs no separate .sig.
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

// pae is DSSE's Pre-Authentication Encoding, which binds the payload type and lengths into what is
// signed so a signature cannot be replayed against a different payload type.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s",
		len(payloadType), payloadType, len(payload), payload))
}
