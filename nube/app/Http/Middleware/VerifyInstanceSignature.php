<?php

namespace App\Http\Middleware;

use App\Models\Instance;
use App\Support\CloudError;
use App\Support\Signature;
use Closure;
use Illuminate\Database\UniqueConstraintViolationException;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Symfony\Component\HttpFoundation\Response;

/**
 * Autenticación de la API de instancia, en el orden del contrato:
 *
 *  1. instancia conocida y activa (unknown_instance);
 *  2. marca de tiempo dentro de ±NUBE_CLOCK_SKEW (timestamp_out_of_window
 *     con serverTime);
 *  3. el cuerpo recibido coincide con X-Capataz-Content-SHA256
 *     (checksum_mismatch);
 *  4. firma HMAC, comparada con hash_equals (invalid_signature);
 *  5. nonce no usado: se inserta solo después de verificar la firma, y la
 *     clave primaria (instancia, nonce) hace que repetirlo falle
 *     (replayed_nonce).
 *
 * El cuerpo nunca se carga entero si es una parte: se vuelca por flujo a un
 * temporal mientras se calcula su SHA-256 (atributo "spool" de la
 * petición). Antes de leerlo se exige un Content-Length acotado.
 */
class VerifyInstanceSignature
{
    /** Tope del cuerpo JSON (metaBlob de 256 KiB en base64url más el resto). */
    private const MAX_JSON_BODY = 300 * 1024;

    public function handle(Request $request, Closure $next): Response
    {
        $h = $request->headers;

        // 1. Instancia.
        $instanceId = (string) $h->get('X-Capataz-Instance', '');
        $instance = $instanceId === '' ? null : Instance::where('instance_id', $instanceId)->where('active', true)->first();
        if ($instance === null) {
            throw new CloudError(401, 'unknown_instance', 'instancia desconocida o inactiva');
        }

        // Formato de los demás headers (sin ellos no hay nada que verificar).
        $ts = (string) $h->get('X-Capataz-Timestamp', '');
        $nonce = (string) $h->get('X-Capataz-Nonce', '');
        $bodyHex = (string) $h->get('X-Capataz-Content-SHA256', '');
        $sig = (string) $h->get('X-Capataz-Signature', '');
        if (! preg_match('/^-?\d{1,19}$/', $ts) || ! preg_match('/^[A-Za-z0-9_-]{22}$/', $nonce)
            || ! preg_match('/^[0-9a-f]{64}$/', $bodyHex) || ! preg_match('/^[A-Za-z0-9_-]{43}$/', $sig)) {
            throw new CloudError(401, 'unauthorized', 'credenciales incompletas');
        }

        // 2. Reloj.
        $now = now()->getTimestamp();
        if (abs($now - (int) $ts) > config('capataz.clock_skew')) {
            throw new CloudError(401, 'timestamp_out_of_window', 'marca de tiempo fuera de la ventana', ['serverTime' => $now]);
        }

        // 3. Cuerpo.
        $actual = $this->readBody($request);
        if (! hash_equals($bodyHex, $actual)) {
            $this->dropSpool($request);
            throw new CloudError(400, 'checksum_mismatch', 'el cuerpo no coincide con X-Capataz-Content-SHA256');
        }

        // 4. Firma, sobre el destino tal como viajó (ruta y consulta sin
        // normalizar).
        $target = (string) $request->server('REQUEST_URI', '');
        $canonical = Signature::canonical($request->getMethod(), $target, $ts, $nonce, $bodyHex);
        if (! hash_equals(Signature::sign($instance->rawKey(), $canonical), $sig)) {
            $this->dropSpool($request);
            throw new CloudError(401, 'invalid_signature', 'firma inválida');
        }

        // 5. Nonce.
        try {
            DB::table('nonces')->insert(['instance_id' => $instance->id, 'nonce' => $nonce, 'seen_at' => now()]);
        } catch (UniqueConstraintViolationException) {
            $this->dropSpool($request);
            throw new CloudError(401, 'replayed_nonce', 'nonce repetido');
        }

        $request->attributes->set('instance', $instance);
        try {
            return $next($request);
        } finally {
            $this->dropSpool($request); // si el controlador no lo publicó
        }
    }

    /** Devuelve el SHA-256 hex del cuerpo; los PUT se vuelcan a un temporal. */
    private function readBody(Request $request): string
    {
        $len = $request->headers->get('Content-Length');
        $hasBody = $len !== null && $len !== '0';
        if (! $hasBody) {
            return hash('sha256', '');
        }
        if (! ctype_digit((string) $len)) {
            throw CloudError::invalid('Content-Length inválido');
        }
        $len = (int) $len;

        if ($request->isMethod('PUT')) {
            if ($len > config('capataz.limits.max_part_size')) {
                throw new CloudError(413, 'payload_too_large', 'parte demasiado grande');
            }

            return $this->spool($request, $len);
        }
        if ($len > self::MAX_JSON_BODY) {
            throw new CloudError(413, 'payload_too_large', 'cuerpo demasiado grande');
        }

        return hash('sha256', $request->getContent());
    }

    /** Vuelca el cuerpo por flujo a data_dir/tmp calculando su SHA-256. */
    private function spool(Request $request, int $len): string
    {
        $dir = config('capataz.data_dir').'/tmp';
        if (! is_dir($dir) && ! @mkdir($dir, 0700, true) && ! is_dir($dir)) {
            throw new CloudError(500, 'internal_error', 'error interno');
        }
        $path = $dir.'/in-'.bin2hex(random_bytes(8));
        $out = fopen($path, 'xb');
        $in = $request->getContent(true); // php://input como flujo
        $ctx = hash_init('sha256');
        $chunk = config('capataz.stream_chunk');
        $total = 0;
        try {
            while (! feof($in) && $total <= $len) {
                $buf = fread($in, $chunk);
                if ($buf === false) {
                    break;
                }
                hash_update($ctx, $buf);
                fwrite($out, $buf);
                $total += strlen($buf);
            }
        } finally {
            fclose($out);
        }
        $request->attributes->set('spool', ['path' => $path, 'size' => $total]);
        if ($total !== $len) {
            $this->dropSpool($request);
            throw CloudError::invalid('el cuerpo no mide lo que dice Content-Length');
        }

        return hash_final($ctx);
    }

    private function dropSpool(Request $request): void
    {
        $spool = $request->attributes->get('spool');
        if ($spool !== null && is_file($spool['path'])) {
            @unlink($spool['path']);
        }
        $request->attributes->remove('spool');
    }
}
