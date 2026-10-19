package pushgw

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// RFC 8291 (Message Encryption for Web Push) with the aes128gcm content coding of RFC 8188,
// the one scheme every Gotalk client decrypts (gateway docs/payload.md).
const (
	// MaxPlaintext is the largest plaintext, padding included, a push may carry.
	MaxPlaintext = 2048
	// PadTo is the block plaintext is padded to, to blunt length leakage.
	PadTo = 256

	recordSize  = 4096
	keyIDLength = 65
	authLength  = 16
)

// ValidateDeviceKeys checks a subscription's p256dh (uncompressed P-256 point) and auth
// secret (16 bytes).
func ValidateDeviceKeys(p256dh, auth []byte) error {
	if len(p256dh) != keyIDLength || p256dh[0] != 0x04 {
		return errors.New("p256dh must be a 65-byte uncompressed P-256 public key")
	}
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil {
		return errors.New("p256dh is not a point on the P-256 curve")
	}
	if len(auth) != authLength {
		return errors.New("auth must be 16 bytes")
	}
	return nil
}

// Padding returns how many zero bytes to add after a plaintext of n bytes: up to the next
// multiple of PadTo, never past MaxPlaintext.
func Padding(n int) int {
	if n >= MaxPlaintext {
		return 0
	}
	padded := min((n+PadTo-1)/PadTo*PadTo, MaxPlaintext)
	return padded - n
}

// Encrypt builds the RFC 8291 body for a device: one record, the plaintext padded per
// Padding, with a fresh sender key and salt.
func Encrypt(p256dh, auth, plaintext []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return EncryptWith(as, salt, p256dh, auth, plaintext, Padding(len(plaintext)))
}

// EncryptWith is Encrypt with a given sender key, salt and padding (test vectors).
func EncryptWith(as *ecdh.PrivateKey, salt, p256dh, auth, plaintext []byte, padding int) ([]byte, error) {
	if err := ValidateDeviceKeys(p256dh, auth); err != nil {
		return nil, err
	}
	if len(plaintext)+padding > MaxPlaintext {
		return nil, fmt.Errorf("plaintext is %d bytes with padding; the limit is %d", len(plaintext)+padding, MaxPlaintext)
	}
	ua, _ := ecdh.P256().NewPublicKey(p256dh)
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	cek, nonce, err := contentKeys(shared, auth, p256dh, asPublic, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	record := make([]byte, 0, len(plaintext)+1+padding)
	record = append(append(record, plaintext...), 0x02)
	record = append(record, make([]byte, padding)...)
	header := make([]byte, 0, 16+4+1+keyIDLength)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, keyIDLength)
	header = append(header, asPublic...)
	return gcm.Seal(header, nonce, record, nil), nil
}

// Decrypt opens an RFC 8291 body with a device's private key and auth secret, as a client
// does. The server only uses it in tests.
func Decrypt(ua *ecdh.PrivateKey, auth, body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("body is shorter than the header")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if len(body) < 21+idlen {
		return nil, errors.New("body is shorter than the key ID")
	}
	asPublic, ciphertext := body[21:21+idlen], body[21+idlen:]
	if rs < 18 || len(ciphertext) > int(rs) {
		return nil, errors.New("body has more than one record")
	}
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, fmt.Errorf("key ID is not a P-256 public key: %w", err)
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := contentKeys(shared, auth, ua.PublicKey().Bytes(), asPublic, salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(cek)
	if err != nil {
		return nil, err
	}
	record, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}
	record = bytes.TrimRight(record, "\x00")
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		return nil, errors.New("the record has no 0x02 delimiter")
	}
	return record[:len(record)-1], nil
}

// contentKeys derives the content-encryption key and nonce (RFC 8291 §3.4, RFC 8188 §2.2).
func contentKeys(shared, auth, uaPublic, asPublic, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, auth)
	if err != nil {
		return nil, nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
