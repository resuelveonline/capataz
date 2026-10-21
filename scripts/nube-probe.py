#!/usr/bin/env python3
"""Comprobaciones de Capataz Nube en el servidor real (hosting compartido).

  scripts/nube-probe.py --base-url https://sync.softmx.net \
      --instance capataz-prueba --key-file ~/capataz-prueba.key [--size-mib 64]

Pensado para una instancia de prueba aparte (capataz-prueba), que se
desactiva al terminar con `artisan capataz:instance:disable`. Sube un
paquete aleatorio (no es CPK1 válido: la nube no lo descifra), lo descarga y
comprueba, y lo revoca al final aunque algo falle.

La clave (base64url de 32 bytes, tal como la imprime capataz:instance:add)
se lee de un archivo con permisos 0600 o más cerrados; con otros, el script
no corre. Nunca se imprimen la clave, el token ni el tokenHash: solo el
linkId, códigos de estado, tamaños y tiempos.

Solo biblioteca estándar (Python 3.9 o posterior).
"""

import argparse
import base64
import hashlib
import hmac
import http.client
import json
import os
import stat
import sys
import time
import urllib.parse
from datetime import datetime, timedelta, timezone

UA = "capataz-nube-probe/1"
MiB = 1 << 20


def b64url(b):
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def read_key(path):
    st = os.stat(path)
    if not stat.S_ISREG(st.st_mode):
        sys.exit("El archivo de clave no es un archivo regular.")
    if st.st_mode & 0o177:
        sys.exit("El archivo de clave tiene permisos %o; deben ser 0600 o más cerrados (chmod 600)."
                 % stat.S_IMODE(st.st_mode))
    with open(path, "r", encoding="ascii") as f:
        text = f.read().strip()
    try:
        key = base64.urlsafe_b64decode(text + "=" * (-len(text) % 4))
    except ValueError:
        key = b""
    if len(key) != 32 or b64url(key) != text:
        sys.exit("El archivo de clave no contiene 32 bytes en base64url.")
    return key


class Resp:
    def __init__(self, status, headers, body, ttfb=None, total=None):
        self.status, self.headers, self.body = status, headers, body
        self.ttfb, self.total = ttfb, total

    def header(self, name):
        return self.headers.get(name.lower())

    def cloud_error(self):
        """El código de un CloudError del contrato, o None si el cuerpo no lo es."""
        if "application/json" not in (self.header("Content-Type") or ""):
            return None
        try:
            d = json.loads(self.body)
        except ValueError:
            return None
        if isinstance(d, dict) and isinstance(d.get("code"), str) and "message" in d:
            return d["code"]
        return None

    def json(self):
        try:
            return json.loads(self.body)
        except ValueError:
            return None

    def snippet(self):
        """Primeros 80 caracteres del cuerpo, en una línea y solo imprimibles."""
        text = self.body[:400].decode("utf-8", "replace")
        text = "".join(c if c.isprintable() else " " for c in text)
        return " ".join(text.split())[:80]

    def describe(self):
        code = self.cloud_error()
        return "%d %s" % (self.status, code) if code else "%d (sin CloudError)" % self.status


class Nube:
    def __init__(self, base_url, instance, key, timeout):
        u = urllib.parse.urlsplit(base_url.rstrip("/"))
        if u.scheme not in ("http", "https") or not u.hostname or u.query or u.fragment:
            sys.exit("--base-url debe ser http(s)://host[:puerto][/ruta], sin consulta.")
        self.scheme, self.host, self.port = u.scheme, u.hostname, u.port
        self.prefix = u.path
        self.instance, self.key, self.timeout = instance, key, timeout

    def _conn(self):
        cls = http.client.HTTPSConnection if self.scheme == "https" else http.client.HTTPConnection
        return cls(self.host, self.port, timeout=self.timeout)

    def request(self, method, path, body=b"", headers=None, signed=False, sink=None):
        """Hace una petición. Con sink, el cuerpo de la respuesta se entrega por
        trozos a sink(chunk) en lugar de guardarse, y se mide el tiempo hasta
        el primer byte del cuerpo y el total."""
        target = self.prefix + path
        h = {"User-Agent": UA, "Accept": "*/*"}
        h.update(headers or {})
        if signed:
            ts = str(int(time.time()))
            nonce = b64url(os.urandom(16))
            body_hex = hashlib.sha256(body).hexdigest()
            canonical = "\n".join([method, target, ts, nonce, body_hex])
            sig = b64url(hmac.new(self.key, canonical.encode(), hashlib.sha256).digest())
            h.update({
                "X-Capataz-Instance": self.instance,
                "X-Capataz-Timestamp": ts,
                "X-Capataz-Nonce": nonce,
                "X-Capataz-Content-SHA256": body_hex,
                "X-Capataz-Signature": sig,
            })
        if body or method in ("POST", "PUT"):
            h["Content-Length"] = str(len(body))
        conn = self._conn()
        try:
            t0 = time.monotonic()
            conn.request(method, target, body=body or None, headers=h)
            r = conn.getresponse()
            hdrs = {k.lower(): v for k, v in r.getheaders()}
            if sink is None:
                data = r.read()
                return Resp(r.status, hdrs, data, total=time.monotonic() - t0)
            ttfb = None
            while True:
                chunk = r.read(256 * 1024)
                if not chunk:
                    break
                if ttfb is None:
                    ttfb = time.monotonic() - t0
                sink(chunk)
            return Resp(r.status, hdrs, b"", ttfb=ttfb, total=time.monotonic() - t0)
        finally:
            conn.close()


class Report:
    def __init__(self):
        self.failed = 0

    def ok(self, n, msg):
        print("[PASA]  %s. %s" % (n, msg))

    def fail(self, n, msg, cause):
        self.failed += 1
        print("[FALLA] %s. %s" % (n, msg))
        print("        causa probable: %s" % cause)

    def warn(self, n, msg):
        print("[AVISO] %s. %s" % (n, msg))

    def skip(self, n, msg):
        self.failed += 1
        print("[OMITIDA] %s. %s" % (n, msg))

    def info(self, msg):
        print("        %s" % msg)


def signed_cause(resp):
    code = resp.cloud_error()
    if code is None:
        if resp.status in (403, 406):
            return "respuesta del WAF (Imunify360/ModSecurity): HTML %d sin CloudError" % resp.status
        return "la respuesta no es de la aplicación (¿proxy, LiteSpeed o PHP con error fatal?)"
    return {
        "unknown_instance": "instanceId no dado de alta o desactivado",
        "invalid_signature": "clave equivocada o la ruta/consulta llega alterada (REQUEST_URI tras la reescritura)",
        "timestamp_out_of_window": "reloj del equipo desfasado respecto al servidor",
        "replayed_nonce": "nonce repetido (no debería pasar con nonces aleatorios)",
        "checksum_mismatch": "el cuerpo llega alterado o incompleto (proxy o WAF que lo reescribe)",
        "unauthorized": "faltan cabeceras X-Capataz-* (¿las quita un proxy?)",
        "payload_too_large": "límite de tamaño del servidor o de NUBE_MAX_PART_SIZE",
        "quota_exceeded": "cupo de la instancia agotado",
        "rate_limited": "límite de peticiones",
        "internal_error": "error interno de la aplicación (revisar storage/logs en el servidor)",
    }.get(code, "error de la aplicación: %s" % code)


def steps(nube, rep, pkg, part_size, link_id, token, opened):
    """Comprobaciones 2 a 6. Vuelve en cuanto una deja sin sentido las siguientes."""
    size = len(pkg)

    # 2. Abrir y subir las partes.
    body = json.dumps({
        "linkId": link_id,
        "tokenHash": b64url(hashlib.sha256(token.encode()).digest()),
        "expiresAt": (datetime.now(timezone.utc) + timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "maxDownloads": None,
        "cipherVersion": "cpk1",
        "encryptedSize": size,
        "partSize": part_size,
        "metaBlob": b64url(os.urandom(64)),
    }, separators=(",", ":")).encode()
    r = nube.request("POST", "/v1/links", body, {"Content-Type": "application/json"}, signed=True)
    if r.status != 201:
        rep.fail(2, "POST /v1/links %s" % r.describe(), signed_cause(r))
        return
    opened.append(link_id)
    nparts = -(-size // part_size)
    times = []
    for n in range(1, nparts + 1):
        part = pkg[(n - 1) * part_size:n * part_size]
        r = nube.request("PUT", "/v1/links/%s/parts/%d" % (link_id, n), part,
                         {"Content-Type": "application/octet-stream"}, signed=True)
        receipt = r.json() if r.status == 200 else None
        if (isinstance(receipt, dict) and receipt.get("partNumber") == n and receipt.get("size") == len(part)
                and receipt.get("sha256") == hashlib.sha256(part).hexdigest()):
            times.append((n, len(part), r.total))
            continue
        what = "PUT de la parte %d (%d bytes)" % (n, len(part))
        if r.status == 200:
            rep.fail(2, what + ": 200 con un recibo que no cuadra", "la aplicación guardó otra cosa (revisar el servidor)")
        elif r.cloud_error() is None:
            rep.fail(2, what + ": %d sin CloudError" % r.status,
                     "respuesta del WAF (Imunify360) o del servidor web" if r.status in (403, 406)
                     else "respuesta que no es de la aplicación (servidor web, PHP o proxy)")
        else:
            rep.fail(2, what + ": " + r.describe(), signed_cause(r))
        rep.info("cuerpo: %r" % r.snippet())
        return
    rep.ok(2, "%d partes subidas con su recibo" % nparts)
    for n, plen, t in times:
        rep.info("parte %d: %d bytes en %.2f s (%.1f MiB/s)" % (n, plen, t, plen / MiB / t if t else 0))

    # 3. complete y active.
    r = nube.request("POST", "/v1/links/%s/complete" % link_id,
                     json.dumps({"ciphertextSha256": hashlib.sha256(pkg).hexdigest()}).encode(),
                     {"Content-Type": "application/json"}, signed=True)
    g = nube.request("GET", "/v1/links/%s" % link_id, signed=True)
    state = (g.json() or {}).get("state") if g.status == 200 else None
    if r.status == 200 and (r.json() or {}).get("state") == "active" and state == "active":
        rep.ok(3, "complete 200 en %.2f s; GET /v1/links/%s dice active" % (r.total, link_id))
    else:
        rep.fail(3, "complete %s; estado consultado: %s" % (r.describe(), state), signed_cause(r))
        return

    blob = "/s/%s/blob" % token

    # 4. Blob completo, comparado por trozos sin guardarlo otra vez.
    seen = {"pos": 0, "same": True}

    def compare(chunk):
        p = seen["pos"]
        if seen["same"] and pkg[p:p + len(chunk)] != chunk:
            seen["same"] = False
        seen["pos"] = p + len(chunk)

    full = nube.request("GET", blob, sink=compare)
    full_ok = full.status == 200 and seen["same"] and seen["pos"] == size
    if full_ok:
        rep.ok(4, "blob completo: 200, %d bytes idénticos al original" % size)
    elif full.status != 200:
        rep.fail(4, "blob completo: %s" % full.describe(), "la descarga pública no responde como debe")
    else:
        rep.fail(4, "blob completo: %d bytes recibidos, %s" % (
            seen["pos"], "contenido distinto" if not seen["same"] else "tamaño distinto"),
            "la respuesta llega alterada o cortada (compresión, proxy o límite de tiempo de PHP)")

    # 5. Range entre partes, dos intervalos y HEAD.
    a, b = part_size - 1000, part_size + 999
    problems = []
    r = nube.request("GET", blob, headers={"Range": "bytes=%d-%d" % (a, b)})
    if r.status != 206 or r.body != pkg[a:b + 1] or r.header("Content-Range") != "bytes %d-%d/%d" % (a, b, size):
        problems.append("Range %d-%d: %d, %d bytes" % (a, b, r.status, len(r.body)))
    r = nube.request("GET", blob, headers={"Range": "bytes=0-1,4-5"})
    if r.status != 416 or r.header("Content-Range") != "bytes */%d" % size:
        problems.append("dos intervalos: %d (se esperaba 416)" % r.status)
    r = nube.request("HEAD", blob)
    if r.status != 200 or r.header("Content-Length") != str(size) or r.body:
        problems.append("HEAD: %d, Content-Length %s" % (r.status, r.header("Content-Length")))
    r = nube.request("HEAD", blob, headers={"Range": "bytes=%d-%d" % (a, b)})
    if r.status != 206 or r.header("Content-Length") != str(b - a + 1) or r.body:
        problems.append("HEAD con Range: %d, Content-Length %s" % (r.status, r.header("Content-Length")))
    if problems:
        rep.fail(5, "; ".join(problems), "el servidor web altera Range/HEAD o la aplicación responde mal")
    else:
        rep.ok(5, "Range que cruza las partes 1 y 2: 206 con los bytes correctos; dos intervalos: 416; "
                  "HEAD: 200 y 206 sin cuerpo")

    # 6. Buffering, con los tiempos de la descarga completa del punto 4.
    if not full_ok:
        rep.skip(6, "sin blob completo no se mide el buffering")
        return
    ttfb, total = full.ttfb or 0.0, full.total or 0.0
    rep.info("blob de %d bytes: primer byte a %.2f s, total %.2f s (%.1f MiB/s)" % (
        size, ttfb, total, size / MiB / total if total else 0))
    if total < 1.0:
        rep.warn(6, "sin conclusión: la descarga tardó menos de 1 s; usa un --size-mib mayor (p. ej. 64)")
    elif ttfb >= 0.8 * total:
        rep.fail(6, "el primer byte llega al %d %% del tiempo total" % (100 * ttfb / total),
                 "LiteSpeed acumula la respuesta antes de enviarla (buffering)")
    else:
        rep.ok(6, "el primer byte llega al %d %% del tiempo total: la respuesta fluye" % (100 * ttfb / total))


def main():
    ap = argparse.ArgumentParser(description="Comprobaciones de Capataz Nube contra un servidor real.")
    ap.add_argument("--base-url", required=True, help="p. ej. https://sync.softmx.net")
    ap.add_argument("--instance", required=True, help="instanceId de prueba (p. ej. capataz-prueba)")
    ap.add_argument("--key-file", required=True, help="archivo 0600 con la clave en base64url")
    ap.add_argument("--size-mib", type=float, default=20, help="tamaño del paquete de prueba (por defecto 20)")
    ap.add_argument("--part-mib", type=int, default=8, help="tamaño de parte (por defecto 8)")
    ap.add_argument("--timeout", type=float, default=300, help="segundos por petición (por defecto 300)")
    args = ap.parse_args()

    key = read_key(os.path.expanduser(args.key_file))
    nube = Nube(args.base_url, args.instance, key, args.timeout)
    part_size = args.part_mib * MiB
    size = int(args.size_mib * MiB)
    if size <= part_size + 2000:
        sys.exit("--size-mib debe ser mayor que --part-mib para probar un Range entre partes.")
    rep = Report()

    print("Capataz Nube: %s://%s%s%s  instancia %s" % (
        nube.scheme, nube.host, ":%d" % nube.port if nube.port else "", nube.prefix, args.instance))
    print("Paquete de prueba: %d bytes en partes de %d" % (size, part_size))

    # 1. GET firmado, sin y con consulta.
    try:
        r0 = nube.request("GET", "/v1/instance", signed=True)
        r1 = nube.request("GET", "/v1/links?state=active&limit=2", signed=True)
    except OSError as e:
        rep.fail(1, "no hubo conexión (%s)" % type(e).__name__, "URL equivocada, DNS o TLS")
        return 1
    if r0.status == 200 and r1.status == 200:
        rep.ok(1, "GET /v1/instance y GET /v1/links con consulta firmados: 200 y 200")
    elif r0.status == 200 and r1.cloud_error() == "invalid_signature":
        rep.fail(1, "GET sin consulta 200, con consulta %s" % r1.describe(),
                 "la consulta llega alterada a REQUEST_URI (reescritura de LiteSpeed/.htaccess)")
    elif r0.status == 200:
        rep.fail(1, "GET sin consulta 200, con consulta %s" % r1.describe(), signed_cause(r1))
    else:
        rep.fail(1, "GET /v1/instance %s" % r0.describe(), signed_cause(r0))
        if r0.cloud_error() == "timestamp_out_of_window":
            server = (r0.json() or {}).get("serverTime")
            if isinstance(server, int):
                rep.info("desfase: %+d s respecto al servidor" % (server - int(time.time())))
        rep.info("sin firma válida no se puede seguir")
        return 1

    pkg = os.urandom(size)
    link_id = "probe-" + os.urandom(8).hex()
    token = b64url(os.urandom(32))
    print("linkId: %s" % link_id)
    opened = []
    try:
        steps(nube, rep, pkg, part_size, link_id, token, opened)
    except OSError as e:
        rep.fail("-", "se cortó la conexión (%s)" % type(e).__name__,
                 "red, límite de tiempo o el servidor cerró la conexión")
    finally:
        # 7. Limpieza: revocar siempre que se haya abierto, aunque algo fallara.
        if opened:
            try:
                r = nube.request("DELETE", "/v1/links/%s" % link_id, signed=True)
                p = nube.request("GET", "/s/%s/blob" % token)
                if r.status == 204 and p.status == 410:
                    rep.ok(7, "DELETE 204 y la descarga pública responde 410")
                else:
                    rep.fail(7, "DELETE %s; descarga pública %d" % (r.describe(), p.status), signed_cause(r))
            except OSError as e:
                rep.fail(7, "no se pudo revocar (%s)" % type(e).__name__,
                         "revócalo a mano: DELETE /v1/links/%s" % link_id)

    print("Resultado: %s" % ("todo PASA" if rep.failed == 0 else "%d sin pasar" % rep.failed))
    return 0 if rep.failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
