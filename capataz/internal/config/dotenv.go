package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var keyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadDotEnv lee un archivo .env y define las variables que todavía no
// existan en el entorno (el entorno real siempre gana). Solo se usa en
// desarrollo, con la opción -env-file; en producción systemd entrega las
// variables con EnvironmentFile=.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	vars, err := parseDotEnv(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range vars {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// parseDotEnv admite:
//
//	# comentarios y líneas vacías
//	CLAVE=valor            (espacios alrededor recortados)
//	CLAVE=valor # comentario
//	CLAVE="valor con # y \"escapes\"\n"
//	CLAVE='literal sin escapes'
//	export CLAVE=valor
//
// Los errores indican el número de línea, nunca el contenido.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	vars := make(map[string]string)
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyRE.MatchString(key) {
			return nil, fmt.Errorf("línea %d: se esperaba CLAVE=valor", n)
		}
		val, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("línea %d (%s): %w", n, key, err)
		}
		vars[key] = val
	}
	return vars, sc.Err()
}

func parseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("comilla simple sin cerrar")
		}
		return raw[1 : end+1], checkTrailing(raw[end+2:])
	case '"':
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			c := raw[i]
			switch {
			case c == '"':
				return b.String(), checkTrailing(raw[i+1:])
			case c == '\\' && i+1 < len(raw):
				i++
				switch raw[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default: // \" \\ y cualquier otro: el carácter tal cual
					b.WriteByte(raw[i])
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("comilla doble sin cerrar")
	default:
		// Sin comillas, " #" inicia un comentario; un # pegado es parte del valor.
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = raw[:i]
		}
		return strings.TrimSpace(raw), nil
	}
}

// checkTrailing acepta tras la comilla de cierre solo espacios o un comentario.
func checkTrailing(rest string) error {
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.HasPrefix(rest, "#") {
		return nil
	}
	return fmt.Errorf("texto inesperado después de la comilla de cierre")
}
