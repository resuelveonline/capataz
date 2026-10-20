package cpk

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// vectors es docs/vectores-cpk1.json (claves fijas solo para pruebas).
type vectors struct {
	KeyHex         string `json:"keyHex"`
	SaltHex        string `json:"saltHex"`
	NoncePrefixHex string `json:"noncePrefixHex"`
	Content        struct {
		DerivedKeyHex string `json:"derivedKeyHex"`
	} `json:"content"`
	Cases []struct {
		Name            string `json:"name"`
		ChunkSizeLog2   uint8  `json:"chunkSizeLog2"`
		PlaintextLength int64  `json:"plaintextLength"`
		PlaintextSha256 string `json:"plaintextSha256"`
		PackageLength   int64  `json:"packageLength"`
		PackageSha256   string `json:"packageSha256"`
		PackageHex      string `json:"packageHex"`
	} `json:"cases"`
	Meta struct {
		DerivedKeyHex string `json:"derivedKeyHex"`
		NonceHex      string `json:"nonceHex"`
		PlaintextUtf8 string `json:"plaintextUtf8"`
		BlobHex       string `json:"blobHex"`
		BlobBase64url string `json:"blobBase64url"`
	} `json:"meta"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../../docs/vectores-cpk1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) != 5 {
		t.Fatalf("se esperaban 5 casos, hay %d", len(v.Cases))
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (v vectors) key(t *testing.T) Key {
	var k Key
	copy(k[:], unhex(t, v.KeyHex))
	return k
}

func (v vectors) params(t *testing.T, log2 uint8) params {
	p := params{log2: log2}
	copy(p.salt[:], unhex(t, v.SaltHex))
	copy(p.prefix[:], unhex(t, v.NoncePrefixHex))
	return p
}

// plaintext es el contenido de los vectores: byte i = i mod 251.
func plaintext(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestVectorsEncrypt(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			pt := plaintext(c.PlaintextLength)
			if got := fmt.Sprintf("%x", sha256.Sum256(pt)); got != c.PlaintextSha256 {
				t.Fatalf("contenido en claro distinto del vector: %s", got)
			}
			var out bytes.Buffer
			n, err := encrypt(&out, bytes.NewReader(pt), c.PlaintextLength, v.key(t), v.params(t, c.ChunkSizeLog2))
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(out.Bytes()); got != c.PackageHex {
				t.Errorf("paquete distinto de packageHex\n got  %s…\n want %s…", got[:min(96, len(got))], c.PackageHex[:96])
			}
			if n != c.PackageLength || PackageSize(c.PlaintextLength, c.ChunkSizeLog2) != c.PackageLength {
				t.Errorf("largo %d / PackageSize %d; quiero %d", n, PackageSize(c.PlaintextLength, c.ChunkSizeLog2), c.PackageLength)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(out.Bytes())); got != c.PackageSha256 {
				t.Errorf("SHA-256 del paquete %s; quiero %s", got, c.PackageSha256)
			}
		})
	}
}

func TestVectorsDecrypt(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			n, err := Decrypt(&out, bytes.NewReader(unhex(t, c.PackageHex)), v.key(t))
			if err != nil {
				t.Fatal(err)
			}
			if n != c.PlaintextLength || !bytes.Equal(out.Bytes(), plaintext(c.PlaintextLength)) {
				t.Errorf("claro de %d bytes distinto del esperado", n)
			}
		})
	}
}

func TestVectorsDerivedKey(t *testing.T) {
	v := loadVectors(t)
	sub, err := contentKey(v.key(t), unhex(t, v.SaltHex))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(sub) != v.Content.DerivedKeyHex {
		t.Errorf("subclave de contenido %x; quiero %s", sub, v.Content.DerivedKeyHex)
	}
}

// casePackage devuelve el paquete del vector por nombre.
func casePackage(t *testing.T, v vectors, name string) []byte {
	for _, c := range v.Cases {
		if c.Name == name {
			return unhex(t, c.PackageHex)
		}
	}
	t.Fatalf("no existe el caso %s", name)
	return nil
}

func TestDecryptRejects(t *testing.T) {
	v := loadVectors(t)
	const blk = 1024 + TagSize                          // bloque cifrado con chunkSizeLog2 = 10
	tres := casePackage(t, v, "tres_bloques_con_resto") // 32 + 1040 + 1040 + 468
	dos := casePackage(t, v, "dos_bloques_exactos")     // 32 + 1040 + 1040
	corto := casePackage(t, v, "corto")

	mod := func(src []byte, f func([]byte) []byte) []byte {
		return f(bytes.Clone(src))
	}
	flip := func(off int) func([]byte) []byte {
		return func(b []byte) []byte { b[off] ^= 0x01; return b }
	}
	set := func(off int, val byte) func([]byte) []byte {
		return func(b []byte) []byte { b[off] = val; return b }
	}
	otherKey := v.key(t)
	otherKey[0] ^= 0xff

	tests := []struct {
		name string
		pkg  []byte
		key  Key
		want error
	}{
		{"bit cambiado en el cifrado (bloque 1)", mod(tres, flip(HeaderSize+blk+7)), v.key(t), ErrAuth},
		{"bit cambiado en la etiqueta del último bloque", mod(tres, flip(len(tres)-1)), v.key(t), ErrAuth},
		{"bit cambiado en la sal de la cabecera", mod(tres, flip(12)), v.key(t), ErrAuth},
		{"bit cambiado en el prefijo de nonce", mod(tres, flip(30)), v.key(t), ErrAuth},
		{"truncado en límite de bloque", dos[:HeaderSize+blk], v.key(t), ErrAuth},
		{"truncado a mitad de bloque", tres[:len(tres)-5], v.key(t), ErrAuth},
		{"bloques 0 y 1 intercambiados", mod(tres, func(b []byte) []byte {
			b0 := bytes.Clone(b[HeaderSize : HeaderSize+blk])
			copy(b[HeaderSize:], b[HeaderSize+blk:HeaderSize+2*blk])
			copy(b[HeaderSize+blk:], b0)
			return b
		}), v.key(t), ErrAuth},
		{"bloque duplicado", append(bytes.Clone(dos[:HeaderSize+blk]), dos[HeaderSize:]...), v.key(t), ErrAuth},
		{"bytes extra al final", append(bytes.Clone(corto), make([]byte, 20)...), v.key(t), ErrAuth},
		{"clave incorrecta", corto, otherKey, ErrAuth},

		{"magia distinta", mod(corto, set(3, '2')), v.key(t), ErrHeader},
		{"versión 2", mod(corto, set(4, 2)), v.key(t), ErrHeader},
		{"chunkSizeLog2 = 9", mod(corto, set(5, 9)), v.key(t), ErrHeader},
		{"chunkSizeLog2 = 25", mod(corto, set(5, 25)), v.key(t), ErrHeader},
		{"chunkSizeLog2 = 0", mod(corto, set(5, 0)), v.key(t), ErrHeader},
		{"banderas distintas de cero", mod(corto, set(6, 1)), v.key(t), ErrHeader},
		{"reservado[0] distinto de cero", mod(corto, set(7, 1)), v.key(t), ErrHeader},
		{"reservado[1] distinto de cero", mod(corto, set(8, 0x80)), v.key(t), ErrHeader},

		{"vacío", nil, v.key(t), ErrTruncated},
		{"cabecera incompleta", corto[:20], v.key(t), ErrTruncated},
		{"solo cabecera (sin bloques)", corto[:HeaderSize], v.key(t), ErrTruncated},
		{"cola de menos de 16 bytes", tres[:HeaderSize+2*blk+10], v.key(t), ErrTruncated},
		{"40 bytes (menos de 48)", corto[:40], v.key(t), ErrTruncated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decrypt(io.Discard, bytes.NewReader(tt.pkg), tt.key)
			if !errors.Is(err, tt.want) {
				t.Errorf("error = %v; quiero %v", err, tt.want)
			}
		})
	}
}

// Rango de chunkSizeLog2 que un lector DEBE aceptar: 10 a 24.
func TestDecryptAcceptsChunkRange(t *testing.T) {
	k, _ := NewKey()
	for _, log2 := range []uint8{10, 16, 20, 24} {
		p, _ := randomParams()
		p.log2 = log2
		pt := plaintext(3000)
		var pkg bytes.Buffer
		if _, err := encrypt(&pkg, bytes.NewReader(pt), int64(len(pt)), k, p); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if _, err := Decrypt(&out, &pkg, k); err != nil || !bytes.Equal(out.Bytes(), pt) {
			t.Errorf("log2=%d: %v", log2, err)
		}
	}
}

func TestEncryptLengthMismatch(t *testing.T) {
	k, _ := NewKey()
	pt := plaintext(100)
	for name, declared := range map[string]int64{"menos de lo declarado": 101, "más de lo declarado": 99, "negativo": -1} {
		t.Run(name, func(t *testing.T) {
			_, err := Encrypt(io.Discard, bytes.NewReader(pt), declared, k)
			if !errors.Is(err, ErrLength) {
				t.Errorf("error = %v; quiero ErrLength", err)
			}
		})
	}
}

// Ciclo con el tamaño de producción (1 MiB) y un archivo de 3 MiB + 1 byte:
// cuatro bloques, el último de un solo byte.
func TestRoundTrip3MiBPlus1(t *testing.T) {
	const size = 3<<20 + 1
	pt := make([]byte, size)
	rand.Read(pt)
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	var pkg bytes.Buffer
	n, err := Encrypt(&pkg, bytes.NewReader(pt), size, k)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(HeaderSize + size + TagSize*4); n != want || int64(pkg.Len()) != want || PackageSize(size, 20) != want {
		t.Errorf("largo del paquete %d (búfer %d); quiero %d", n, pkg.Len(), want)
	}
	if pkg.Bytes()[5] != DefaultChunkSizeLog2 {
		t.Errorf("chunkSizeLog2 = %d", pkg.Bytes()[5])
	}

	dst := filepath.Join(t.TempDir(), "claro.bin")
	got, err := DecryptFile(dst, bytes.NewReader(pkg.Bytes()), k, size)
	if err != nil || got != size {
		t.Fatalf("DecryptFile: %d, %v", got, err)
	}
	back, _ := os.ReadFile(dst)
	if sha256.Sum256(back) != sha256.Sum256(pt) {
		t.Error("el ciclo no devuelve el mismo contenido")
	}
}

// El tamaño producido es siempre 32 + largo + 16*n, en los bordes de bloque.
func TestPackageSizeFormula(t *testing.T) {
	k, _ := NewKey()
	const log2 = 10
	c := int64(1) << log2
	for _, l := range []int64{0, 1, c - 1, c, c + 1, 2*c - 1, 2 * c, 2*c + 1, 7*c + 3} {
		p, _ := randomParams()
		p.log2 = log2
		n, err := encrypt(io.Discard, bytes.NewReader(plaintext(l)), l, k, p)
		if err != nil {
			t.Fatal(err)
		}
		blocks := max(1, (l+c-1)/c)
		if want := HeaderSize + l + TagSize*blocks; n != want || PackageSize(l, log2) != want {
			t.Errorf("largo %d: paquete %d, PackageSize %d; quiero %d", l, n, PackageSize(l, log2), want)
		}
	}
	if got := PackageSize(MaxPayload, DefaultChunkSizeLog2); got != 524_296_032 {
		t.Errorf("tope del paquete %d; quiero 524296032 (§4.4)", got)
	}
}

func TestDecryptFileLeavesNothingOnFailure(t *testing.T) {
	v := loadVectors(t)
	pkg := casePackage(t, v, "tres_bloques_con_resto")
	pkg[len(pkg)-3] ^= 1 // falla el ÚLTIMO bloque: los dos primeros ya se escribieron
	dir := t.TempDir()
	dst := filepath.Join(dir, "salida.bin")
	if _, err := DecryptFile(dst, bytes.NewReader(pkg), v.key(t), -1); !errors.Is(err, ErrAuth) {
		t.Fatalf("error = %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("quedaron archivos tras el fallo: %v", left)
	}

	// Largo esperado distinto: también se rechaza.
	ok := casePackage(t, v, "corto")
	if _, err := DecryptFile(dst, bytes.NewReader(ok), v.key(t), 41); !errors.Is(err, ErrLength) {
		t.Errorf("error = %v; quiero ErrLength", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("no debe existir el archivo de salida")
	}
}

// pattern genera n bytes deterministas sin guardarlos.
type pattern struct{ off, n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.n-p.off)]
	for i := range b {
		b[i] = byte((p.off + int64(i)) * 7 % 251)
	}
	p.off += int64(len(b))
	return len(b), nil
}

// Memoria constante: cifrar y descifrar 64 MiB en flujo (sin guardar ni el
// claro ni el paquete) no debe reservar memoria proporcional al tamaño.
func TestStreamingConstantMemory(t *testing.T) {
	const size = 64 << 20
	k, _ := NewKey()
	pr, pw := io.Pipe()
	go func() {
		_, err := Encrypt(pw, &pattern{n: size}, size, k)
		pw.CloseWithError(err)
	}()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	h := sha256.New()
	n, err := Decrypt(h, pr, k)
	runtime.ReadMemStats(&after)
	if err != nil || n != size {
		t.Fatalf("Decrypt: %d, %v", n, err)
	}

	want := sha256.New()
	io.Copy(want, &pattern{n: size})
	if !bytes.Equal(h.Sum(nil), want.Sum(nil)) {
		t.Error("contenido distinto tras el ciclo en flujo")
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("se reservaron %d MiB para 64 MiB: la memoria no es constante", alloc>>20)
	}
}

func TestKeyNeverPrinted(t *testing.T) {
	k, _ := NewKey()
	secret := k.Base64URL()
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "k", k)
	out := fmt.Sprintf("%v %s %x %+v %#v %q", k, k, k, k, k, k) + logs.String()
	if _, err := json.Marshal(struct{ K Key }{k}); err == nil {
		t.Error("json.Marshal no debe serializar la clave")
	}
	for _, leak := range []string{secret, hex.EncodeToString(k[:]), fmt.Sprint(k[:4])} {
		if strings.Contains(out, leak) {
			t.Errorf("la clave se filtra al imprimir: %s", out)
		}
	}
	back, err := ParseKey(secret)
	if err != nil || back != k || len(secret) != 43 {
		t.Errorf("ParseKey(Base64URL) = %v (largo %d)", err, len(secret))
	}
	if _, err := ParseKey(secret[:42]); err == nil {
		t.Error("ParseKey debe rechazar claves de largo incorrecto")
	}
}
