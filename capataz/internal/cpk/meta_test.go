package cpk

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func TestVectorsMeta(t *testing.T) {
	v := loadVectors(t)
	k := v.key(t)

	sub, err := metaKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(sub) != v.Meta.DerivedKeyHex {
		t.Errorf("subclave de metadatos %x; quiero %s", sub, v.Meta.DerivedKeyHex)
	}

	var nonce [nonceSize]byte
	copy(nonce[:], unhex(t, v.Meta.NonceHex))
	blob, err := encryptMeta(k, nonce, []byte(v.Meta.PlaintextUtf8))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(blob) != v.Meta.BlobHex {
		t.Errorf("metaBlob distinto del vector\n got  %x\n want %s", blob, v.Meta.BlobHex)
	}
	if base64.RawURLEncoding.EncodeToString(blob) != v.Meta.BlobBase64url {
		t.Error("metaBlob en base64url distinto del vector")
	}

	plain, err := DecryptMeta(k, unhex(t, v.Meta.BlobHex))
	if err != nil || string(plain) != v.Meta.PlaintextUtf8 {
		t.Errorf("DecryptMeta = %q, %v", plain, err)
	}
}

func TestMetaRejects(t *testing.T) {
	v := loadVectors(t)
	k := v.key(t)
	blob := unhex(t, v.Meta.BlobHex)
	other := k
	other[31] ^= 1

	flip := func(off int) []byte { b := bytes.Clone(blob); b[off] ^= 1; return b }
	tests := []struct {
		name string
		blob []byte
		key  Key
		want error
	}{
		{"bit cambiado en el nonce", flip(0), k, ErrAuth},
		{"bit cambiado en el cifrado", flip(20), k, ErrAuth},
		{"bit cambiado en la etiqueta", flip(len(blob) - 1), k, ErrAuth},
		{"clave incorrecta", blob, other, ErrAuth},
		{"truncado", blob[:len(blob)-1], k, ErrAuth},
		{"más corto que nonce + etiqueta", blob[:27], k, ErrTruncated},
		{"demasiado grande", make([]byte, MaxMetaBlob+1), k, ErrMetaTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecryptMeta(tt.key, tt.blob); !errors.Is(err, tt.want) {
				t.Errorf("error = %v; quiero %v", err, tt.want)
			}
		})
	}
}

// La subclave de metadatos no es la de contenido: misma K, otro uso.
func TestMetaUsesSeparateSubkey(t *testing.T) {
	v := loadVectors(t)
	if v.Meta.DerivedKeyHex == v.Content.DerivedKeyHex {
		t.Fatal("los vectores tienen la misma subclave para contenido y metadatos")
	}
}

func TestMetaRoundTripAndLimit(t *testing.T) {
	k, _ := NewKey()
	json := []byte(`{"v":1,"kind":"zip","name":"fotos.zip","size":10}`)
	blob, err := EncryptMeta(k, json)
	if err != nil || len(blob) != MetaBlobSize(len(json)) {
		t.Fatalf("EncryptMeta: %d bytes, %v", len(blob), err)
	}
	other, _ := EncryptMeta(k, json)
	if bytes.Equal(blob[:nonceSize], other[:nonceSize]) {
		t.Error("dos metaBlob con el mismo nonce: debe ser aleatorio")
	}
	if back, err := DecryptMeta(k, blob); err != nil || !bytes.Equal(back, json) {
		t.Errorf("ciclo: %q, %v", back, err)
	}

	maxJSON := MaxMetaBlob - nonceSize - TagSize
	if _, err := EncryptMeta(k, make([]byte, maxJSON)); err != nil {
		t.Errorf("justo en el tope: %v", err)
	}
	if _, err := EncryptMeta(k, make([]byte, maxJSON+1)); !errors.Is(err, ErrMetaTooLarge) {
		t.Errorf("un byte sobre el tope: %v", err)
	}
}
