package nube_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
)

// go test ./internal/nube -run TestVectoresNube -update regenera el archivo
// a partir de esta implementación. Después hay que verificarlo de forma
// independiente (scripts/vectores-nube-verify.py) antes de confirmarlo.
var update = flag.Bool("update", false, "regenera docs/vectores-nube.json")

const vectorsFile = "../../../docs/vectores-nube.json"

type tokenVector struct {
	Token     string `json:"token"`
	TokenHash string `json:"tokenHash"`
}

type signVector struct {
	Name          string            `json:"name"`
	Method        string            `json:"method"`
	Target        string            `json:"target"`
	Timestamp     string            `json:"timestamp"`
	Nonce         string            `json:"nonce"`
	BodyEncoding  string            `json:"bodyEncoding"` // "empty", "utf8" o "hex"
	Body          string            `json:"body"`
	ContentSHA256 string            `json:"contentSha256"`
	Canonical     string            `json:"canonical"`
	Signature     string            `json:"signature"`
	Headers       map[string]string `json:"headers"`
}

type vectorsDoc struct {
	Description string        `json:"description"`
	TokenHash   []tokenVector `json:"tokenHash"`
	Signature   struct {
		InstanceID        string       `json:"instanceId"`
		InstanceKeyBase64 string       `json:"instanceKeyBase64url"`
		InstanceKeyHex    string       `json:"instanceKeyHex"`
		Cases             []signVector `json:"cases"`
	} `json:"signature"`
}

func (v signVector) body(t *testing.T) []byte {
	switch v.BodyEncoding {
	case "empty":
		return nil
	case "utf8":
		return []byte(v.Body)
	case "hex":
		b, err := hex.DecodeString(v.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("%s: bodyEncoding %q", v.Name, v.BodyEncoding)
	return nil
}

// buildVectors arma los vectores con entradas fijas y salidas calculadas
// por nube.TokenHash y nube.Sign.
func buildVectors() vectorsDoc {
	var d vectorsDoc
	d.Description = "Vectores de Capataz Nube (api/capataz-nube-api.yaml). tokenHash = " +
		"base64url sin relleno de SHA-256 del texto de 43 caracteres del token, tal como viaja en la URL. " +
		"Firma = base64url sin relleno de HMAC-SHA256(clave de instancia, cadena canónica); la cadena " +
		"canónica son cinco líneas separadas por \\n sin salto final: método, destino (ruta y consulta tal " +
		"como viajan), timestamp, nonce y SHA-256 hex del cuerpo. Claves y tokens de ejemplo, no reales."

	for _, raw := range [][]byte{
		bytes.Repeat([]byte{0}, 32),
		seq(0, 32),
		bytes.Repeat([]byte{0xff}, 32),
	} {
		tok := base64.RawURLEncoding.EncodeToString(raw)
		d.TokenHash = append(d.TokenHash, tokenVector{Token: tok, TokenHash: nube.TokenHash(tok)})
	}

	key := seq(0x40, 32)
	d.Signature.InstanceID = "vectoresnube0000000000000a"
	d.Signature.InstanceKeyBase64 = base64.RawURLEncoding.EncodeToString(key)
	d.Signature.InstanceKeyHex = hex.EncodeToString(key)
	cases := []signVector{
		{Name: "GET con consulta y sin cuerpo", Method: "GET", Target: "/v1/links?state=active&limit=2",
			Timestamp: "1760000000", Nonce: "AAECAwQFBgcICQoLDA0ODw", BodyEncoding: "empty"},
		{Name: "POST con cuerpo JSON", Method: "POST", Target: "/v1/links",
			Timestamp: "1760000060", Nonce: "EBESExQVFhcYGRobHB0eHw", BodyEncoding: "utf8",
			Body: `{"linkId":"enlaceDeEjemplo0001","tokenHash":"` + d.TokenHash[1].TokenHash +
				`","expiresAt":"2025-10-10T09:00:00Z","maxDownloads":null,"cipherVersion":"cpk1",` +
				`"encryptedSize":1048624,"partSize":8388608,"metaBlob":"AAECAwQFBgcICQoL"}`},
		{Name: "PUT de una parte binaria pequeña", Method: "PUT", Target: "/v1/links/enlaceDeEjemplo0001/parts/1",
			Timestamp: "1760000120", Nonce: "ICEiIyQlJicoKSorLC0uLw", BodyEncoding: "hex",
			Body: hex.EncodeToString(append([]byte("CPK1"), seq(0, 28)...))},
	}
	for i := range cases {
		c := &cases[i]
		var body []byte
		if c.BodyEncoding == "utf8" {
			body = []byte(c.Body)
		} else if c.BodyEncoding == "hex" {
			body, _ = hex.DecodeString(c.Body)
		}
		c.ContentSHA256, c.Canonical, c.Signature = nube.Sign(key, c.Method, c.Target, c.Timestamp, c.Nonce, body)
		c.Headers = map[string]string{
			"X-Capataz-Instance":       d.Signature.InstanceID,
			"X-Capataz-Timestamp":      c.Timestamp,
			"X-Capataz-Nonce":          c.Nonce,
			"X-Capataz-Content-SHA256": c.ContentSHA256,
			"X-Capataz-Signature":      c.Signature,
		}
	}
	d.Signature.Cases = cases
	return d
}

// marshalVectors serializa sin escapar &, < ni > para que el archivo se
// lea tal cual (p. ej. el destino "/v1/links?state=active&limit=2").
func marshalVectors(t *testing.T) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildVectors()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func seq(start byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func TestVectoresNube(t *testing.T) {
	if *update {
		if err := os.WriteFile(vectorsFile, marshalVectors(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	var d vectorsDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	if len(d.TokenHash) != 3 || len(d.Signature.Cases) != 3 {
		t.Fatalf("se esperaban 3 tokens y 3 firmas: %d, %d", len(d.TokenHash), len(d.Signature.Cases))
	}
	for _, v := range d.TokenHash {
		if len(v.Token) != 43 || len(v.TokenHash) != 43 {
			t.Errorf("largos: %q %q", v.Token, v.TokenHash)
		}
		if got := nube.TokenHash(v.Token); got != v.TokenHash {
			t.Errorf("tokenHash(%s) = %s; el vector dice %s", v.Token, got, v.TokenHash)
		}
	}
	key, err := nube.ParseKey(d.Signature.InstanceKeyBase64)
	if err != nil || hex.EncodeToString(key) != d.Signature.InstanceKeyHex {
		t.Fatalf("clave de instancia del vector: %v", err)
	}
	for _, v := range d.Signature.Cases {
		bodyHex, canonical, sig := nube.Sign(key, v.Method, v.Target, v.Timestamp, v.Nonce, v.body(t))
		if bodyHex != v.ContentSHA256 || canonical != v.Canonical || sig != v.Signature {
			t.Errorf("%s: no coincide\n sha %s / %s\n firma %s / %s", v.Name, bodyHex, v.ContentSHA256, sig, v.Signature)
		}
		h := v.Headers
		if h["X-Capataz-Signature"] != v.Signature || h["X-Capataz-Content-SHA256"] != v.ContentSHA256 ||
			h["X-Capataz-Nonce"] != v.Nonce || h["X-Capataz-Timestamp"] != v.Timestamp ||
			h["X-Capataz-Instance"] != d.Signature.InstanceID {
			t.Errorf("%s: headers incoherentes", v.Name)
		}
		if _, err := strconv.ParseInt(v.Timestamp, 10, 64); err != nil || len(v.Nonce) != 22 || len(v.Signature) != 43 {
			t.Errorf("%s: formato de timestamp, nonce o firma", v.Name)
		}
	}
	// Los vectores deben seguir siendo los que genera esta implementación.
	if !bytes.Equal(data, marshalVectors(t)) {
		t.Error("docs/vectores-nube.json no coincide con buildVectors (¿cambió el generador?)")
	}
}
