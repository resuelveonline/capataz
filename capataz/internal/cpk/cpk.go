// Package cpk implementa Capataz Package v1 (CPK1), el formato cifrado de
// los enlaces. La especificación normativa es docs/formato-paquete-cifrado.md:
// si este código y ese texto discrepan, manda el texto.
//
// Cifrar y descifrar trabajan en flujo con un búfer de un bloque: la memoria
// no depende del tamaño del archivo.
package cpk

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
)

const (
	magic   = "CPK1"
	version = 0x01

	// HeaderSize es el largo de la cabecera en claro (§4.1).
	HeaderSize = 32
	// TagSize es la etiqueta de AES-GCM que acompaña a cada bloque.
	TagSize = 16
	// KeySize es el largo de la clave del enlace K.
	KeySize = 32

	// DefaultChunkSizeLog2 es el bloque que escribe Capataz: 1 MiB.
	DefaultChunkSizeLog2 = 20
	// Rango que un lector DEBE aceptar (§4.1).
	MinChunkSizeLog2 = 10
	MaxChunkSizeLog2 = 24

	// MaxPayload es el tope de contenido en claro por enlace (§4.4).
	MaxPayload = 500 << 20 // 524 288 000 bytes

	saltSize   = 16
	prefixSize = 7
	nonceSize  = 12

	infoContent = "capataz-cpk1/content"
)

var (
	// ErrHeader: la cabecera no es CPK1 v1 válida.
	ErrHeader = errors.New("cpk: cabecera inválida")
	// ErrAuth: un bloque no pasó la verificación (alterado, reordenado,
	// truncado en un límite de bloque o clave incorrecta).
	ErrAuth = errors.New("cpk: verificación fallida")
	// ErrTruncated: el paquete termina a mitad de una estructura.
	ErrTruncated = errors.New("cpk: paquete truncado")
	// ErrLength: el contenido no mide lo declarado al cifrar.
	ErrLength = errors.New("cpk: largo del contenido distinto del declarado")
)

// Key es la clave del enlace. Sus métodos de texto la ocultan para que un
// fmt.Print o slog accidental no la filtre; para obtenerla hay que pedirla
// con Base64URL.
type Key [KeySize]byte

// NewKey genera una clave con crypto/rand.
func NewKey() (Key, error) {
	var k Key
	_, err := rand.Read(k[:])
	return k, err
}

// ParseKey decodifica una clave en base64url sin relleno (43 caracteres).
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return k, errors.New("cpk: clave inválida")
	}
	copy(k[:], b)
	return k, nil
}

// Base64URL devuelve la clave tal como va en el fragmento de la URL.
func (k Key) Base64URL() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

func (Key) String() string       { return "cpk.Key(oculta)" }
func (Key) GoString() string     { return "cpk.Key(oculta)" }
func (Key) LogValue() slog.Value { return slog.StringValue("oculta") }
func (Key) MarshalText() ([]byte, error) {
	return nil, errors.New("cpk: la clave no se serializa")
}

// params son los valores aleatorios de un paquete. Solo las pruebas los
// fijan (vectores); el código de producción usa randomParams.
type params struct {
	log2   uint8
	salt   [saltSize]byte
	prefix [prefixSize]byte
}

func randomParams() (params, error) {
	p := params{log2: DefaultChunkSizeLog2}
	if _, err := rand.Read(p.salt[:]); err != nil {
		return p, err
	}
	_, err := rand.Read(p.prefix[:])
	return p, err
}

// blockCount es n = max(1, ceil(largo / C)) (§4.3).
func blockCount(plainLen int64, log2 uint8) int64 {
	c := int64(1) << log2
	n := (plainLen + c - 1) / c
	return max(n, 1)
}

// PackageSize es el largo exacto del paquete: 32 + largo + 16*n (§4.3).
func PackageSize(plainLen int64, log2 uint8) int64 {
	return HeaderSize + plainLen + TagSize*blockCount(plainLen, log2)
}

// Encrypt cifra exactamente plainLen bytes de src en dst con bloques de
// 1 MiB y sal y prefijo aleatorios. Devuelve los bytes escritos. Si src
// entrega más o menos de plainLen, devuelve ErrLength.
func Encrypt(dst io.Writer, src io.Reader, plainLen int64, k Key) (int64, error) {
	p, err := randomParams()
	if err != nil {
		return 0, err
	}
	return encrypt(dst, src, plainLen, k, p)
}

func encrypt(dst io.Writer, src io.Reader, plainLen int64, k Key, p params) (int64, error) {
	if plainLen < 0 {
		return 0, ErrLength
	}
	if p.log2 < MinChunkSizeLog2 || p.log2 > MaxChunkSizeLog2 {
		return 0, ErrHeader
	}
	n := blockCount(plainLen, p.log2)
	if n > math.MaxUint32+1 {
		return 0, ErrLength // el contador de bloque es de 32 bits
	}

	header := p.header()
	aead, err := contentAEAD(k, p.salt[:])
	if err != nil {
		return 0, err
	}
	written, err := writeAll(dst, header[:], 0)
	if err != nil {
		return written, err
	}

	c := int64(1) << p.log2
	plain := make([]byte, c)
	sealed := make([]byte, 0, c+TagSize)
	var nonce [nonceSize]byte
	copy(nonce[:prefixSize], p.prefix[:])

	for i := int64(0); i < n; i++ {
		size := c
		last := i == n-1
		if last {
			size = plainLen - i*c
		}
		if _, err := io.ReadFull(src, plain[:size]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return written, ErrLength
			}
			return written, err
		}
		setCounter(&nonce, uint32(i), last)
		sealed = aead.Seal(sealed[:0], nonce[:], plain[:size], header[:])
		if written, err = writeAll(dst, sealed, written); err != nil {
			return written, err
		}
	}

	// src no debe tener más bytes de los declarados.
	var extra [1]byte
	if m, _ := io.ReadFull(src, extra[:]); m > 0 {
		return written, ErrLength
	}
	return written, nil
}

// Decrypt descifra un paquete de src hacia dst y devuelve los bytes en
// claro escritos.
//
// Escribe cada bloque en cuanto lo verifica, así que si devuelve error
// dst ya puede tener una parte del claro: quien llama DEBE descartarla
// (DecryptFile lo hace por ti). Esto es lo que permite memoria constante.
func Decrypt(dst io.Writer, src io.Reader, k Key) (int64, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, ErrTruncated
		}
		return 0, err
	}
	p, err := parseHeader(header)
	if err != nil {
		return 0, err
	}
	aead, err := contentAEAD(k, p.salt[:])
	if err != nil {
		return 0, err
	}

	c := int(1) << p.log2
	br := bufio.NewReaderSize(src, 64<<10)
	block := make([]byte, c+TagSize)
	var nonce [nonceSize]byte
	copy(nonce[:prefixSize], p.prefix[:])

	var total int64
	for i := uint64(0); ; i++ {
		m, err := io.ReadFull(br, block)
		var last bool
		switch {
		case err == nil:
			// Bloque lleno: es el último solo si no queda nada detrás.
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return total, perr
			}
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			if m < TagSize {
				// Incluye el paquete sin ningún bloque (solo cabecera).
				return total, ErrTruncated
			}
			last = true
		default:
			return total, err
		}
		if i > math.MaxUint32 {
			return total, ErrAuth
		}
		setCounter(&nonce, uint32(i), last)
		plain, err := aead.Open(block[:0], nonce[:], block[:m], header[:])
		if err != nil {
			return total, ErrAuth
		}
		if total, err = writeAll(dst, plain, total); err != nil {
			return total, err
		}
		if last {
			return total, nil
		}
	}
}

// DecryptFile descifra el paquete src en el archivo dstPath. Escribe en un
// temporal del mismo directorio y solo lo renombra si TODOS los bloques
// verifican y, si wantSize >= 0, el largo en claro coincide. Si falla, no
// queda ningún archivo.
func DecryptFile(dstPath string, src io.Reader, k Key, wantSize int64) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".cpk-descifrado-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op tras el Rename

	n, err := Decrypt(tmp, src, k)
	if err == nil && wantSize >= 0 && n != wantSize {
		err = ErrLength
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), dstPath)
}

func (p params) header() [HeaderSize]byte {
	var h [HeaderSize]byte
	copy(h[0:4], magic)
	h[4] = version
	h[5] = p.log2
	// h[6] banderas y h[7:9] reservado quedan en cero.
	copy(h[9:25], p.salt[:])
	copy(h[25:32], p.prefix[:])
	return h
}

func parseHeader(h [HeaderSize]byte) (params, error) {
	var p params
	switch {
	case string(h[0:4]) != magic,
		h[4] != version,
		h[5] < MinChunkSizeLog2 || h[5] > MaxChunkSizeLog2,
		h[6] != 0,
		h[7] != 0 || h[8] != 0:
		return p, ErrHeader
	}
	p.log2 = h[5]
	copy(p.salt[:], h[9:25])
	copy(p.prefix[:], h[25:32])
	return p, nil
}

// setCounter completa nonce_i = prefijo ‖ be32(i) ‖ último (§4.2).
func setCounter(nonce *[nonceSize]byte, i uint32, last bool) {
	binary.BigEndian.PutUint32(nonce[prefixSize:prefixSize+4], i)
	nonce[nonceSize-1] = 0
	if last {
		nonce[nonceSize-1] = 1
	}
}

// contentKey deriva la subclave de contenido (§3).
func contentKey(k Key, salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, k[:], salt, infoContent, 32)
}

func contentAEAD(k Key, salt []byte) (cipher.AEAD, error) {
	sub, err := contentKey(k, salt)
	if err != nil {
		return nil, err
	}
	return newGCM(sub)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func writeAll(w io.Writer, b []byte, total int64) (int64, error) {
	n, err := w.Write(b)
	total += int64(n)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return total, fmt.Errorf("cpk: escribiendo: %w", err)
	}
	return total, nil
}
