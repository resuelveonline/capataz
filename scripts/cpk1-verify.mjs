#!/usr/bin/env node
// Verificador CPK1 en JavaScript con Web Crypto y sin dependencias: prueba
// que un paquete cifrado por Capataz (Go) se descifra igual que lo hará la
// página de descarga en el navegador (docs/formato-paquete-cifrado.md §7.2).
//
// Uso:
//   node scripts/cpk1-verify.mjs --package P --key-file F --meta M --out O [--meta-out J]
//       --key-file F   archivo con la clave en base64url (43 caracteres), o "-" para stdin.
//                      La clave nunca se acepta como argumento: se vería en `ps`.
//       --meta M       archivo con el metaBlob en base64url (meta.b64).
//       --out O        archivo en claro de salida. Se escribe en un temporal y solo se
//                      publica si TODOS los bloques verifican y el tamaño coincide con
//                      meta.size.
//       --meta-out J   opcional: guarda el JSON de metadatos descifrado (0600).
//   node scripts/cpk1-verify.mjs --vectors docs/vectores-cpk1.json
//       Valida los cinco vectores, el metaBlob y los casos negativos.
//
// Por stdout solo sale un resumen sin nombres (kind, size, sha256, entryCount).

import fs from "node:fs";
import path from "node:path";

const subtle = globalThis.crypto?.subtle;
if (!subtle) fail("Web Crypto no disponible (se necesita Node 19 o posterior)");

const HEADER = 32;
const TAG = 16;
const enc = new TextEncoder();

function fail(msg) {
  console.error(`cpk1-verify: ${msg}`);
  process.exit(1);
}

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith("--")) fail("argumento inesperado");
    out[a.slice(2)] = argv[++i];
    if (out[a.slice(2)] === undefined) fail(`falta el valor de ${a}`);
  }
  return out;
}

function b64urlDecode(s) {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) throw new Error("base64url inválido");
  return new Uint8Array(Buffer.from(s, "base64url"));
}

const hex = (b) => Buffer.from(b).toString("hex");
const unhex = (s) => new Uint8Array(Buffer.from(s, "hex"));

async function sha256hex(chunks) {
  // Web Crypto no hashea en flujo; para el resumen basta node:crypto.
  const { createHash } = await import("node:crypto");
  const h = createHash("sha256");
  for (const c of chunks) h.update(c);
  return h.digest("hex");
}

// HKDF-SHA256 → clave AES-256-GCM de solo descifrado (§3).
async function subkey(K, salt, info) {
  const ikm = await subtle.importKey("raw", K, "HKDF", false, ["deriveBits"]);
  const bits = await subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt, info: enc.encode(info) }, ikm, 256);
  return subtle.importKey("raw", bits, { name: "AES-GCM" }, false, ["decrypt"]);
}

function parseHeader(h) {
  const magic = String.fromCharCode(...h.subarray(0, 4));
  const log2 = h[5];
  if (magic !== "CPK1" || h[4] !== 1) throw new Error("cabecera inválida: magia o versión");
  if (log2 < 10 || log2 > 24) throw new Error("cabecera inválida: chunkSizeLog2 fuera de 10 a 24");
  if (h[6] !== 0 || h[7] !== 0 || h[8] !== 0) throw new Error("cabecera inválida: banderas o reservado");
  return { log2, salt: h.slice(9, 25), prefix: h.slice(25, 32) };
}

// Descifra un paquete leído con readAt(offset, length). Llama a onBlock con
// cada bloque en claro en orden; si algún bloque falla, lanza y quien llama
// DEBE descartar lo ya recibido.
async function decryptPackage(readAt, size, K, onBlock) {
  if (size < HEADER + TAG) throw new Error("paquete truncado");
  const header = readAt(0, HEADER);
  const { log2, salt, prefix } = parseHeader(header);
  const C = 2 ** log2;
  const n = Math.max(1, Math.ceil((size - HEADER) / (C + TAG)));
  const lastLen = size - HEADER - (n - 1) * (C + TAG);
  if (lastLen < TAG || lastLen > C + TAG) throw new Error("paquete truncado");
  if (n > 2 ** 32) throw new Error("demasiados bloques");

  const key = await subkey(K, salt, "capataz-cpk1/content");
  const nonce = new Uint8Array(12);
  nonce.set(prefix, 0);
  const view = new DataView(nonce.buffer);
  let total = 0;
  for (let i = 0; i < n; i++) {
    const last = i === n - 1;
    view.setUint32(7, i, false);
    nonce[11] = last ? 1 : 0;
    const block = readAt(HEADER + i * (C + TAG), last ? lastLen : C + TAG);
    let plain;
    try {
      plain = new Uint8Array(await subtle.decrypt({ name: "AES-GCM", iv: nonce, additionalData: header, tagLength: 128 }, key, block));
    } catch {
      throw new Error(`verificación fallida en el bloque ${i}`);
    }
    await onBlock(plain);
    total += plain.length;
  }
  return total;
}

async function decryptMeta(K, blob) {
  if (blob.length < 12 + TAG) throw new Error("metaBlob truncado");
  if (blob.length > 196608) throw new Error("metaBlob demasiado grande");
  const key = await subkey(K, new Uint8Array(0), "capataz-cpk1/meta");
  try {
    const plain = await subtle.decrypt(
      { name: "AES-GCM", iv: blob.subarray(0, 12), additionalData: enc.encode("capataz-cpk1/meta"), tagLength: 128 },
      key,
      blob.subarray(12),
    );
    return new Uint8Array(plain);
  } catch {
    throw new Error("verificación fallida en los metadatos");
  }
}

function readKey(spec) {
  const text = (spec === "-" ? fs.readFileSync(0, "utf8") : fs.readFileSync(spec, "utf8")).trim();
  let K;
  try {
    K = b64urlDecode(text);
  } catch {
    fail("clave inválida");
  }
  if (text.length !== 43 || K.length !== 32) fail("clave inválida: se esperan 32 bytes en base64url (43 caracteres)");
  return K;
}

async function verifyFile(args) {
  for (const k of ["package", "key-file", "meta", "out"]) if (!args[k]) fail(`falta --${k}`);
  const K = readKey(args["key-file"]);

  let meta;
  try {
    meta = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(await decryptMeta(K, b64urlDecode(fs.readFileSync(args.meta, "utf8").trim()))));
  } catch (e) {
    fail(e.message.startsWith("verificación") || e.message.startsWith("metaBlob") ? e.message : "metadatos ilegibles");
  }
  if (meta.v !== 1 || !["file", "zip"].includes(meta.kind) || !Number.isSafeInteger(meta.size)) fail("metadatos con formato inesperado");

  const fd = fs.openSync(args.package, "r");
  const size = fs.fstatSync(fd).size;
  const readAt = (off, len) => {
    const b = Buffer.alloc(len);
    let got = 0;
    while (got < len) {
      const n = fs.readSync(fd, b, got, len - got, off + got);
      if (n === 0) throw new Error("paquete truncado");
      got += n;
    }
    return new Uint8Array(b.buffer, b.byteOffset, len);
  };

  const outDir = path.dirname(path.resolve(args.out));
  const tmp = path.join(outDir, `.cpk1-verify-${process.pid}-${Date.now()}.tmp`);
  const out = fs.openSync(tmp, "wx", 0o600);
  const { createHash } = await import("node:crypto");
  const h = createHash("sha256");
  let total;
  try {
    total = await decryptPackage(readAt, size, K, (plain) => {
      fs.writeSync(out, plain);
      h.update(plain);
    });
    if (total !== meta.size) throw new Error("el tamaño en claro no coincide con meta.size");
    fs.fsyncSync(out);
    fs.closeSync(out);
    fs.renameSync(tmp, args.out);
  } catch (e) {
    try { fs.closeSync(out); } catch {}
    fs.rmSync(tmp, { force: true });
    fail(e.message); // no se publica nada parcial
  } finally {
    fs.closeSync(fd);
  }
  if (args["meta-out"]) fs.writeFileSync(args["meta-out"], JSON.stringify(meta), { mode: 0o600 });

  const summary = { ok: true, kind: meta.kind, size: total, sha256: h.digest("hex"), packageSize: size };
  if (meta.kind === "zip") summary.entryCount = meta.entryCount;
  console.log(JSON.stringify(summary));
}

async function verifyVectors(file) {
  const v = JSON.parse(fs.readFileSync(file, "utf8"));
  const K = unhex(v.keyHex);
  let checks = 0;
  let failures = 0;
  const check = (ok, what) => {
    checks++;
    if (!ok) {
      failures++;
      console.error(`  FALLO  ${what}`);
    }
  };
  const decryptBuf = async (pkg) => {
    const parts = [];
    await decryptPackage((o, l) => pkg.subarray(o, o + l), pkg.length, K, (p) => parts.push(p));
    return Buffer.concat(parts);
  };
  const rejects = async (pkg, what) => {
    try {
      await decryptBuf(pkg);
      check(false, `${what}: se aceptó`);
    } catch {
      check(true, what);
    }
  };

  for (const c of v.cases) {
    const pkg = unhex(c.packageHex);
    const plain = await decryptBuf(pkg);
    const expected = Buffer.from(Array.from({ length: c.plaintextLength }, (_, i) => i % 251));
    check(plain.length === c.plaintextLength && plain.equals(expected), `${c.name}: claro`);
    check((await sha256hex([plain])) === c.plaintextSha256, `${c.name}: SHA-256 del claro`);
    check((await sha256hex([pkg])) === c.packageSha256 && pkg.length === c.packageLength, `${c.name}: paquete`);

    const C = 2 ** c.chunkSizeLog2;
    const flip = (off) => { const b = pkg.slice(); b[off] ^= 1; return b; };
    const set = (off, val) => { const b = pkg.slice(); b[off] = val; return b; };
    await rejects(flip(pkg.length - 1), `${c.name}: bit cambiado en el cifrado`);
    await rejects(flip(12), `${c.name}: bit cambiado en la cabecera`);
    await rejects(set(5, 9), `${c.name}: chunkSizeLog2 = 9`);
    await rejects(set(5, 25), `${c.name}: chunkSizeLog2 = 25`);
    await rejects(set(6, 1), `${c.name}: banderas`);
    await rejects(set(7, 1), `${c.name}: reservado`);
    const blocks = Math.max(1, Math.ceil(c.plaintextLength / C));
    if (blocks >= 2) {
      await rejects(pkg.slice(0, HEADER + C + TAG), `${c.name}: truncado en límite de bloque`);
      const b = pkg.slice();
      b.set(pkg.subarray(HEADER + C + TAG, HEADER + 2 * (C + TAG)), HEADER);
      b.set(pkg.subarray(HEADER, HEADER + C + TAG), HEADER + C + TAG);
      await rejects(b, `${c.name}: bloques intercambiados`);
    }
  }
  const meta = new TextDecoder().decode(await decryptMeta(K, unhex(v.meta.blobHex)));
  check(meta === v.meta.plaintextUtf8, "metaBlob: descifrado");
  check(hex(b64urlDecode(v.meta.blobBase64url)) === v.meta.blobHex, "metaBlob: base64url");
  const badMeta = unhex(v.meta.blobHex);
  badMeta[20] ^= 1;
  try {
    await decryptMeta(K, badMeta);
    check(false, "metaBlob alterado: se aceptó");
  } catch {
    check(true, "metaBlob alterado");
  }

  console.log(JSON.stringify({ ok: failures === 0, checks, failures }));
  if (failures) process.exit(1);
}

const args = parseArgs(process.argv.slice(2));
try {
  if (args.vectors) await verifyVectors(args.vectors);
  else await verifyFile(args);
} catch (e) {
  fail(e.message);
}
