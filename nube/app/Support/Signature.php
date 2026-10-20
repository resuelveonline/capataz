<?php

namespace App\Support;

/**
 * Firma de la API de instancia y tokenHash (api/capataz-nube-api.yaml).
 * Solo hash_hmac, hash y hash_equals: el hosting no tiene sodium.
 */
final class Signature
{
    /** Cadena canónica: cinco líneas separadas por "\n", sin salto final. */
    public static function canonical(string $method, string $target, string $timestamp, string $nonce, string $bodySha256Hex): string
    {
        return implode("\n", [$method, $target, $timestamp, $nonce, $bodySha256Hex]);
    }

    /** base64url(HMAC-SHA256(clave, cadena canónica)), sin relleno. */
    public static function sign(string $rawKey, string $canonical): string
    {
        return self::b64url(hash_hmac('sha256', $canonical, $rawKey, true));
    }

    /**
     * tokenHash = base64url(SHA-256(texto de 43 caracteres del token)), con
     * el texto tal como viaja en la URL, no sus 32 bytes decodificados.
     */
    public static function tokenHash(string $token): string
    {
        return self::b64url(hash('sha256', $token, true));
    }

    public static function b64url(string $bytes): string
    {
        return rtrim(strtr(base64_encode($bytes), '+/', '-_'), '=');
    }

    public static function b64urlDecode(string $s): string|false
    {
        if (! preg_match('/^[A-Za-z0-9_-]*$/', $s)) {
            return false;
        }

        return base64_decode(strtr($s, '-_', '+/'), true);
    }
}
