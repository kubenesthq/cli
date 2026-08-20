package sshx

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// keyMaterial wraps raw private-key bytes so they cannot leave the process by
// accident. Every rendering and serialization path is overridden: printing it
// yields a redaction marker and marshaling it is an error, never the bytes.
// The raw bytes are reachable only inside this file.
type keyMaterial struct {
	raw []byte
}

func newKeyMaterial(raw []byte) *keyMaterial { return &keyMaterial{raw: raw} }

func (k *keyMaterial) signer() (ssh.Signer, error) {
	return ssh.ParsePrivateKey(k.raw)
}

func (k *keyMaterial) signerWithPassphrase(pw []byte) (ssh.Signer, error) {
	return ssh.ParsePrivateKeyWithPassphrase(k.raw, pw)
}

// String implements fmt.Stringer: %v and %s print the marker, not the key.
func (k *keyMaterial) String() string { return "[redacted ssh private key]" }

// GoString implements fmt.GoStringer: %#v prints the marker, not the struct.
func (k *keyMaterial) GoString() string { return "[redacted ssh private key]" }

// Format covers every other fmt verb, including %+v and field-by-field dumps.
func (k *keyMaterial) Format(f fmt.State, verb rune) {
	fmt.Fprint(f, "[redacted ssh private key]")
}

var errKeyNotSerializable = errors.New("sshx: private key material must never be serialized")

// MarshalJSON refuses: key material must never be encoded into a request body.
func (k *keyMaterial) MarshalJSON() ([]byte, error) { return nil, errKeyNotSerializable }

// MarshalText refuses likewise (covers YAML, XML and text-based encoders).
func (k *keyMaterial) MarshalText() ([]byte, error) { return nil, errKeyNotSerializable }

// MarshalBinary refuses likewise (covers gob and binary encoders).
func (k *keyMaterial) MarshalBinary() ([]byte, error) { return nil, errKeyNotSerializable }
