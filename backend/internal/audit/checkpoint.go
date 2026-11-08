package audit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/damhau/quai/backend/internal/store/db"
)

// CheckpointType is the type of a checkpoint in an export, which an event does not have.
const CheckpointType = "checkpoint"

// Checkpoint is a signed statement that the log reached seq with hash: the worker signs one
// every hour and at shutdown (docs/design/09-security-and-compliance.md#audit-log). Key is the ID
// of the Ed25519 key that signed it (KeyID), and Signature its signature, in base64, of the
// checkpoint without the signature in RFC 8785's canonical JSON.
type Checkpoint struct {
	Type      string `json:"type"`
	Seq       int64  `json:"seq"`
	Hash      string `json:"hash"`
	At        string `json:"at"`
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

// CheckpointOf returns a stored checkpoint.
func CheckpointOf(c db.AuditCheckpoint) Checkpoint {
	return Checkpoint{Type: CheckpointType, Seq: c.Seq, Hash: c.Hash, At: Time(c.At.Time), Key: c.Key, Signature: c.Signature}
}

// payload is what the signature covers.
func (c Checkpoint) payload() ([]byte, error) {
	b, err := json.Marshal(struct {
		Type string `json:"type"`
		Seq  int64  `json:"seq"`
		Hash string `json:"hash"`
		At   string `json:"at"`
		Key  string `json:"key"`
	}{CheckpointType, c.Seq, c.Hash, c.At, c.Key})
	if err != nil {
		return nil, err
	}
	return jcs.Transform(b)
}

// Sign signs a checkpoint of the event at seq with hash, at a time kept to the millisecond.
func Sign(key ed25519.PrivateKey, seq int64, hash string, at time.Time) (Checkpoint, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return Checkpoint{}, errors.New("audit: not an Ed25519 key")
	}
	c := Checkpoint{Type: CheckpointType, Seq: seq, Hash: hash, At: Time(at.Truncate(time.Millisecond)), Key: KeyID(pub)}
	p, err := c.payload()
	if err != nil {
		return Checkpoint{}, err
	}
	c.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, p))
	return c, nil
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify checks that the checkpoint is well formed and signed by one of keys.
func (c Checkpoint) Verify(keys Keys) error {
	if c.Type != CheckpointType {
		return fmt.Errorf("its type is %q, not %s", c.Type, CheckpointType)
	}
	if c.Seq < 1 || !hexHash.MatchString(c.Hash) {
		return errors.New("it names no event: its seq or its hash is malformed")
	}
	if t, err := time.Parse(AtFormat, c.At); err != nil || Time(t) != c.At {
		return fmt.Errorf("its time %q is not in the form %s", c.At, AtFormat)
	}
	pub, ok := keys[c.Key]
	if !ok {
		return fmt.Errorf("it is signed with key %s, which was not given", c.Key)
	}
	sig, err := base64.StdEncoding.DecodeString(c.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("its signature is malformed")
	}
	p, err := c.payload()
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, p, sig) {
		return errors.New("its signature does not verify")
	}
	return nil
}

// Keys are the public keys checkpoints are verified with, by ID.
type Keys map[string]ed25519.PublicKey

// KeyID identifies an Ed25519 public key: the SHA-256 of its 32 bytes, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Add adds public keys.
func (k Keys) Add(pubs ...ed25519.PublicKey) {
	for _, p := range pubs {
		k[KeyID(p)] = p
	}
}

// ParsePrivateKey reads an Ed25519 private key in PEM, PKCS #8 ("PRIVATE KEY", as
// `openssl genpkey -algorithm ed25519` writes it).
func ParsePrivateKey(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("the checkpoint signing key is not a PEM \"PRIVATE KEY\" (openssl genpkey -algorithm ed25519)")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the checkpoint signing key: %w", err)
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the checkpoint signing key is not an Ed25519 key")
	}
	return key, nil
}

// ParsePublicKeys reads Ed25519 public keys in PEM ("PUBLIC KEY", as `openssl pkey -pubout`
// writes them), one or more.
func ParsePublicKeys(b []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("a PEM %q, not a \"PUBLIC KEY\"", block.Type)
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("a public key that is not Ed25519")
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, errors.New("no PEM \"PUBLIC KEY\" (openssl pkey -pubout)")
	}
	return keys, nil
}

// MarshalPublicKey writes a public key as ParsePublicKeys reads it.
func MarshalPublicKey(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
