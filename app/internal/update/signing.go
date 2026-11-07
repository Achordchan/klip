package update

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
)

// ReleaseKey is the public half of the release key (base64), which every
// manifest the program takes must be signed with. Its private half is the
// GitHub secret KLIP_MANIFEST_KEY: lost, no installed copy could be updated
// any more; known to somebody else, they could install anything on every
// copy. A variable only for the end to end test's build (-ldflags -X, with a
// key of its own); a release never sets it.
var ReleaseKey = "i46UGHQmeqVJRz4KmrziCfyUKWbnLr1C6blQqZyWFtw="

// Envelope is manifest.signed.json: the bytes of manifest.json, and an
// Ed25519 signature over exactly those bytes by the release key. From 1.0.12
// on the program reads only this, and only with a signature that its own
// public key accepts, whichever way it was fetched: the mirrors that serve
// users who cannot reach GitHub are run by others, and a manifest they
// changed (another package, with its checksum) would otherwise be installed
// without asking. manifest.json stays beside it, unchanged, for 1.0.11.
//
// What a signature cannot stop is a mirror serving an older envelope that
// was valid once: it is never installed (an older or equal version is "up to
// date"), and the routes are raced, the newest version wins (see Check).
type Envelope struct {
	Manifest  string `json:"manifest"`  // base64 of manifest.json
	Signature string `json:"signature"` // base64 of the Ed25519 signature
}

// SignManifest makes the envelope of manifest with key.
func SignManifest(key ed25519.PrivateKey, manifest []byte) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("not an Ed25519 private key")
	}
	if _, err := ParseManifest(manifest); err != nil {
		return nil, err
	}
	env := Envelope{
		Manifest:  base64.StdEncoding.EncodeToString(manifest),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, manifest)),
	}
	return json.MarshalIndent(env, "", "  ")
}

var errBadSignature = errors.New("更新清单的签名无效")

// OpenEnvelope checks the signature of an envelope with pub and returns the
// manifest it carries.
func OpenEnvelope(pub ed25519.PublicKey, data []byte) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("no update key")
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, errBadSignature
	}
	manifest, err1 := base64.StdEncoding.DecodeString(env.Manifest)
	sig, err2 := base64.StdEncoding.DecodeString(env.Signature)
	if err1 != nil || err2 != nil || len(manifest) == 0 || !ed25519.Verify(pub, manifest, sig) {
		return nil, errBadSignature
	}
	return manifest, nil
}

// ParsePublicKey reads the public key the program is built with: the base64
// of its 32 bytes.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not an Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// ParsePrivateKeyPEM reads a private key as `openssl genpkey -algorithm
// ed25519` writes it (PKCS #8, PEM).
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return priv, nil
}
