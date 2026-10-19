package cpk

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

const (
	// MaxMetaBlob es el tamaño máximo del metaBlob ya decodificado (§6.1).
	MaxMetaBlob = 192 << 10 // 196 608 bytes

	infoMeta = "capataz-cpk1/meta"
	aadMeta  = "capataz-cpk1/meta"
)

// ErrMetaTooLarge: el metaBlob superaría MaxMetaBlob.
var ErrMetaTooLarge = errors.New("cpk: metadatos demasiado grandes")

// EncryptMeta cifra el JSON de metadatos (§6):
// metaBlob = nonce (12 bytes aleatorios) ‖ AES-256-GCM-Seal(subclaveMeta, nonce, json, AAD).
// El nonce es aleatorio porque cada K cifra un único bloque de metadatos.
func EncryptMeta(k Key, plaintext []byte) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	return encryptMeta(k, nonce, plaintext)
}

// MetaBlobSize es el largo del metaBlob para un JSON de n bytes.
func MetaBlobSize(n int) int { return nonceSize + n + TagSize }

func encryptMeta(k Key, nonce [nonceSize]byte, plaintext []byte) ([]byte, error) {
	if MetaBlobSize(len(plaintext)) > MaxMetaBlob {
		return nil, ErrMetaTooLarge
	}
	aead, err := metaAEAD(k)
	if err != nil {
		return nil, err
	}
	out := make([]byte, nonceSize, MetaBlobSize(len(plaintext)))
	copy(out, nonce[:])
	return aead.Seal(out, nonce[:], plaintext, []byte(aadMeta)), nil
}

// DecryptMeta verifica y descifra un metaBlob.
func DecryptMeta(k Key, blob []byte) ([]byte, error) {
	if len(blob) > MaxMetaBlob {
		return nil, ErrMetaTooLarge
	}
	if len(blob) < nonceSize+TagSize {
		return nil, ErrTruncated
	}
	aead, err := metaAEAD(k)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, blob[:nonceSize], blob[nonceSize:], []byte(aadMeta))
	if err != nil {
		return nil, ErrAuth
	}
	return plain, nil
}

// metaKey deriva la subclave de metadatos con sal vacía (§3): HKDF usa
// entonces 32 bytes en cero, igual que Web Crypto.
func metaKey(k Key) ([]byte, error) {
	return hkdf.Key(sha256.New, k[:], nil, infoMeta, 32)
}

func metaAEAD(k Key) (cipher.AEAD, error) {
	sub, err := metaKey(k)
	if err != nil {
		return nil, err
	}
	return newGCM(sub)
}
