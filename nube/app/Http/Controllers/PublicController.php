<?php

namespace App\Http\Controllers;

use App\Models\Link;
use App\Services\Lifecycle;
use App\Support\BlobStreamer;
use App\Support\CloudError;
use App\Support\CloudLinkView;
use App\Support\Signature;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Http\Response;
use Illuminate\Support\Facades\DB;
use Symfony\Component\HttpFoundation\StreamedResponse;

/**
 * Descarga pública (sin firma). Estados (contrato v0.1.1):
 *   token desconocido o failed → 404; uploading → 409 link_not_ready;
 *   expired → 410 link_expired; revoked → 410 link_revoked;
 *   exhausted → 410 downloads_exhausted (salvo continuaciones en el margen).
 */
class PublicController
{
    /** Cabeceras comunes de todo lo público. */
    public const HEADERS = [
        'Cache-Control' => 'no-store',
        'Referrer-Policy' => 'no-referrer',
        'X-Robots-Tag' => 'noindex, nofollow',
        'X-Content-Type-Options' => 'nosniff',
    ];

    /** GET /s/{token}: la misma página para cualquier token (HTML y JS en la Fase 2d). */
    public function page(): Response
    {
        $html = <<<'HTML'
<!doctype html>
<html lang="es">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Capataz</title></head>
<body><main><h1>Descarga</h1><p>Página de descarga en preparación.</p></main></body>
</html>
HTML;

        return response($html, 200, self::HEADERS + [
            'Content-Type' => 'text/html; charset=utf-8',
            'Content-Security-Policy' => "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
        ]);
    }

    /** GET /s/{token}/meta. */
    public function meta(Request $request, string $token): JsonResponse
    {
        $link = $this->find($token);
        $this->rejectUnavailable($link);
        if ($link->state === Link::EXHAUSTED) {
            throw new CloudError(410, 'downloads_exhausted', 'el enlace agotó sus descargas', headers: self::HEADERS);
        }

        return response()->json([
            'state' => 'active',
            'expiresAt' => CloudLinkView::date($link->expires_at),
            'encryptedSize' => $link->encrypted_size,
            'cipherVersion' => $link->cipher_version,
            'metaBlob' => $link->meta_blob,
            'downloadsRemaining' => $link->max_downloads === null ? null : max(0, $link->max_downloads - $link->downloads),
        ], 200, self::HEADERS);
    }

    /** GET /s/{token}/blob: el paquete por flujo desde sus partes. */
    public function blob(Request $request, string $token): Response|StreamedResponse
    {
        $link = $this->find($token);
        $this->rejectUnavailable($link);
        if ($request->headers->has('Range')) {
            // Range llega en el commit siguiente.
            throw new CloudError(416, 'invalid_request', 'intervalo no satisfacible', headers: ['Content-Range' => 'bytes */'.$link->encrypted_size]);
        }
        $start = 0;
        $end = $link->encrypted_size - 1;
        // Laravel atiende HEAD con la ruta GET: mismas cabeceras y estado,
        // sin cuerpo y sin contar como descarga.
        $head = $request->isMethod('HEAD');
        if ($link->state === Link::EXHAUSTED || (! $head && ! $this->countDownload($link))) {
            throw new CloudError(410, 'downloads_exhausted', 'el enlace agotó sus descargas', headers: self::HEADERS);
        }

        return $this->streamed($link, $start, $end, 200, $head);
    }

    /**
     * Cuenta una descarga de forma atómica. Devuelve false si el cupo ya
     * estaba agotado. La que agota el cupo se sirve y deja el enlace en
     * exhausted (las continuaciones siguen durante el margen).
     */
    private function countDownload(Link $link): bool
    {
        if ($link->max_downloads === null) {
            Link::whereKey($link->id)->where('state', Link::ACTIVE)->increment('downloads');

            return true;
        }
        $won = Link::whereKey($link->id)->where('state', Link::ACTIVE)
            ->whereColumn('downloads', '<', 'max_downloads')
            ->update(['downloads' => DB::raw('downloads + 1'), 'updated_at' => now()]);
        if ($won === 0) {
            return false;
        }
        Link::whereKey($link->id)->where('state', Link::ACTIVE)->whereColumn('downloads', '>=', 'max_downloads')
            ->update(['state' => Link::EXHAUSTED, 'exhausted_at' => now(), 'updated_at' => now()]);

        return true;
    }

    private function streamed(Link $link, int $start, int $end, int $status, bool $head): Response|StreamedResponse
    {
        $headers = self::HEADERS + [
            'Content-Type' => 'application/octet-stream',
            'Accept-Ranges' => 'bytes',
            'Content-Length' => (string) ($end - $start + 1),
            // Que ningún proxy acumule la respuesta antes de enviarla.
            'X-Accel-Buffering' => 'no',
        ];
        if ($status === 206) {
            $headers['Content-Range'] = "bytes $start-$end/{$link->encrypted_size}";
        }

        if ($head) {
            return response('', $status, $headers);
        }

        return new StreamedResponse(function () use ($link, $start, $end) {
            @set_time_limit(0);
            // Sin búferes de salida (output_buffering del hosting): cada trozo
            // sale en cuanto se lee. Las pruebas capturan con su propio búfer.
            if (! app()->runningUnitTests()) {
                while (ob_get_level() > 0) {
                    ob_end_clean();
                }
            }
            BlobStreamer::stream($link, $start, $end, function (string $buf) {
                echo $buf;
                flush();
            });
        }, $status, $headers);
    }

    private function find(string $token): Link
    {
        $link = preg_match('/^[A-Za-z0-9_-]{43}$/', $token)
            ? Link::where('token_hash', Signature::tokenHash($token))->first()
            : null;
        if ($link === null) {
            throw new CloudError(404, 'not_found', 'no existe', headers: self::HEADERS);
        }

        return Lifecycle::refresh($link);
    }

    /** 404, 409 o 410 según el estado; exhausted lo decide quien llama. */
    private function rejectUnavailable(Link $link): void
    {
        match ($link->state) {
            Link::ACTIVE, Link::EXHAUSTED => null,
            Link::UPLOADING => throw new CloudError(409, 'link_not_ready', 'el enlace aún se está subiendo', headers: self::HEADERS),
            Link::EXPIRED => throw new CloudError(410, 'link_expired', 'el enlace venció', headers: self::HEADERS),
            Link::REVOKED => throw new CloudError(410, 'link_revoked', 'el enlace fue revocado', headers: self::HEADERS),
            default => throw new CloudError(404, 'not_found', 'no existe', headers: self::HEADERS), // failed (C3)
        };
    }
}
