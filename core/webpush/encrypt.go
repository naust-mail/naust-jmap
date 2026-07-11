// Package webpush delivers push messages to a PushSubscription URL:
// message encryption per RFC 8291 and HTTP delivery per RFC 8030
// section 5, with server-side request forgery protection required by
// RFC 8620 section 8.6. It is the sending half of Web Push only - the
// JMAP server acts as an RFC 8030 "application server"; the push
// service and user agent are someone else's.
package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Encryption constants fixed by RFC 8291 section 4 and RFC 8030
// section 7.2: a push service need not accept more than 4096 octets of
// payload, the aes128gcm header for a P-256 key is 86 octets, the
// padding delimiter is 1, and the AEAD tag is 16.
const (
	maxPayload   = 4096
	headerLen    = 16 + 4 + 1 + 65 // salt || rs || idlen || keyid
	MaxPlaintext = maxPayload - headerLen - 1 - 16
)

var (
	// ErrKeys means the subscription's p256dh or auth values are not
	// valid RFC 8291 key material.
	ErrKeys = errors.New("webpush: invalid subscription keys")
	// ErrTooLarge means the plaintext exceeds MaxPlaintext.
	ErrTooLarge = errors.New("webpush: plaintext exceeds the RFC 8030 payload limit")
)

// DecodeKeys decodes and validates a subscription's URL-safe base64
// key material (RFC 8620 section 7.2): p256dh MUST be an uncompressed
// P-256 public key on the curve (RFC 8291 section 7 requires the
// on-curve check) and auth MUST be the 16-octet authentication secret
// of RFC 8291 section 3.2. Both padded and unpadded encodings are
// accepted, as RFC 4648 allows either.
func DecodeKeys(p256dh, auth string) (uaPublic *ecdh.PublicKey, authSecret []byte, err error) {
	pub, err := b64url(p256dh)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: p256dh: %v", ErrKeys, err)
	}
	uaPublic, err = ecdh.P256().NewPublicKey(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: p256dh: %v", ErrKeys, err)
	}
	authSecret, err = b64url(auth)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: auth: %v", ErrKeys, err)
	}
	if len(authSecret) != 16 {
		return nil, nil, fmt.Errorf("%w: auth must be 16 octets", ErrKeys)
	}
	return uaPublic, authSecret, nil
}

func b64url(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// Encrypt encrypts one push message to a subscription's keys per
// RFC 8291, returning the complete aes128gcm-coded body: header (salt,
// record size, application server public key) followed by the single
// encrypted record. A fresh application server key pair and salt are
// generated per message (RFC 8291 section 2).
func Encrypt(uaPublic *ecdh.PublicKey, authSecret, plaintext []byte) ([]byte, error) {
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	return encrypt(uaPublic, authSecret, asKey, salt[:], plaintext)
}

// encrypt is the deterministic core of Encrypt, split out so the
// RFC 8291 section 5 / appendix A test vectors can drive it with the
// published key pair and salt.
func encrypt(uaPublic *ecdh.PublicKey, authSecret []byte, asKey *ecdh.PrivateKey, salt, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, ErrTooLarge
	}
	key, nonce, err := deriveKeyNonce(uaPublic, authSecret, asKey, uaPublic.Bytes(), asKey.PublicKey().Bytes(), salt)
	if err != nil {
		return nil, err
	}

	// RFC 8291 section 4: a single record, the plaintext followed by
	// the 0x02 padding delimiter, sealed with AES-128-GCM.
	record := make([]byte, 0, len(plaintext)+1)
	record = append(record, plaintext...)
	record = append(record, 0x02)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	// The aes128gcm header (RFC 8188 section 2.1): salt, an rs greater
	// than the record plus its 16-octet tag, and the application server
	// public key in uncompressed form as the keyid (RFC 8291 section 4).
	body := make([]byte, 0, headerLen+len(record)+16)
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, maxPayload)
	body = append(body, 65)
	body = append(body, asKey.PublicKey().Bytes()...)
	return gcm.Seal(body, nonce, record, nil), nil
}

// Decrypt is the user-agent side of Encrypt: it opens an aes128gcm
// body using the subscription's private key and authentication secret.
// It exists for JMAP clients and for tests to close the loop; a JMAP
// server never decrypts. Per RFC 8291 section 4, a padding delimiter
// other than 0x02 discards the message.
func Decrypt(uaKey *ecdh.PrivateKey, authSecret, body []byte) ([]byte, error) {
	if len(body) < headerLen+16 {
		return nil, errors.New("webpush: body shorter than the aes128gcm header")
	}
	salt := body[:16]
	if body[20] != 65 {
		return nil, errors.New("webpush: keyid is not an uncompressed P-256 point")
	}
	asPublic, err := ecdh.P256().NewPublicKey(body[21:86])
	if err != nil {
		return nil, err
	}
	key, nonce, err := deriveKeyNonce(asPublic, authSecret, uaKey, uaKey.PublicKey().Bytes(), asPublic.Bytes(), salt)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	record, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		return nil, err
	}
	if len(record) == 0 || record[len(record)-1] != 0x02 {
		return nil, errors.New("webpush: bad padding delimiter")
	}
	return record[:len(record)-1], nil
}

// deriveKeyNonce runs the RFC 8291 section 3.4 derivation: HKDF is
// expanded into discrete HMAC-SHA-256 steps exactly as the section's
// pseudocode does, so no HKDF machinery is needed. peer is the other
// side's public key; uaPublic/asPublic are the raw uncompressed points
// bound into key_info (always user agent first).
func deriveKeyNonce(peer *ecdh.PublicKey, authSecret []byte, private *ecdh.PrivateKey, uaPublic, asPublic, salt []byte) (key, nonce []byte, err error) {
	ecdhSecret, err := private.ECDH(peer)
	if err != nil {
		return nil, nil, err
	}
	// PRK_key = HMAC-SHA-256(auth_secret, ecdh_secret)
	prkKey := hmacSHA256(authSecret, ecdhSecret)
	// IKM = HMAC-SHA-256(PRK_key, key_info || 0x01)
	keyInfo := append([]byte("WebPush: info\x00"), uaPublic...)
	keyInfo = append(keyInfo, asPublic...)
	ikm := hmacSHA256(prkKey, append(keyInfo, 0x01))
	// PRK = HMAC-SHA-256(salt, IKM)
	prk := hmacSHA256(salt, ikm)
	// CEK and NONCE from the RFC 8188 info strings.
	key = hmacSHA256(prk, []byte("Content-Encoding: aes128gcm\x00\x01"))[:16]
	nonce = hmacSHA256(prk, []byte("Content-Encoding: nonce\x00\x01"))[:12]
	return key, nonce, nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
