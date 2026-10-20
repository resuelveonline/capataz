#!/usr/bin/env python3
"""Verifica docs/vectores-nube.json de forma independiente de Go.

Recalcula tokenHash y la firma HMAC de cada caso con la biblioteca estándar
de Python, siguiendo el texto de api/capataz-nube-api.yaml (no el código de
Capataz), y compara con los valores del archivo.

Uso: python3 -I scripts/vectores-nube-verify.py [docs/vectores-nube.json]
"""
import base64
import hashlib
import hmac
import json
import sys


def b64url(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


def b64url_decode(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def main() -> int:
    path = sys.argv[1] if len(sys.argv) > 1 else "docs/vectores-nube.json"
    d = json.load(open(path, encoding="utf-8"))
    bad = 0

    for v in d["tokenHash"]:
        tok = v["token"]
        ok = len(tok) == 43 and len(b64url_decode(tok)) == 32
        # El hash va sobre el texto ASCII del token, no sobre sus 32 bytes.
        ok &= b64url(hashlib.sha256(tok.encode("ascii")).digest()) == v["tokenHash"]
        print(("OK   " if ok else "FALLO"), "tokenHash", tok[:8] + "…")
        bad += not ok

    sig = d["signature"]
    key = b64url_decode(sig["instanceKeyBase64url"])
    if key.hex() != sig["instanceKeyHex"] or len(key) != 32:
        print("FALLO clave de instancia")
        bad += 1
    for c in sig["cases"]:
        enc = c["bodyEncoding"]
        body = b"" if enc == "empty" else c["body"].encode("utf-8") if enc == "utf8" else bytes.fromhex(c["body"])
        body_hex = hashlib.sha256(body).hexdigest()
        canonical = "\n".join([c["method"], c["target"], c["timestamp"], c["nonce"], body_hex])
        signature = b64url(hmac.new(key, canonical.encode("utf-8"), hashlib.sha256).digest())
        h = c["headers"]
        ok = (body_hex == c["contentSha256"] and canonical == c["canonical"] and signature == c["signature"]
              and h["X-Capataz-Content-SHA256"] == body_hex and h["X-Capataz-Signature"] == signature
              and h["X-Capataz-Nonce"] == c["nonce"] and h["X-Capataz-Timestamp"] == c["timestamp"]
              and h["X-Capataz-Instance"] == sig["instanceId"]
              and len(base64.urlsafe_b64decode(c["nonce"] + "==")) == 16 and not canonical.endswith("\n"))
        print(("OK   " if ok else "FALLO"), "firma:", c["name"])
        bad += not ok

    print(f"== {'todo correcto' if bad == 0 else f'{bad} fallos'}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
