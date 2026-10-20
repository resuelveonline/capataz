<?php

namespace App\Http\Controllers;

use App\Models\Link;
use App\Models\LinkPart;
use App\Services\Lifecycle;
use App\Support\CloudError;
use App\Support\PartStore;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

class PartsController
{
    /**
     * PUT /v1/links/{linkId}/parts/{n}: guarda o sustituye la parte n. El
     * middleware ya volcó el cuerpo a un temporal y comprobó su SHA-256;
     * aquí se exige que mida exactamente lo que le toca a la parte n (todas
     * partSize salvo la última) y se publica con rename.
     */
    public function put(Request $request, string $linkId, string $n): JsonResponse
    {
        $link = Lifecycle::refresh(LinksController::findFor($request, $linkId));
        self::requireUploading($link);

        if (! ctype_digit($n) || (int) $n < 1 || (int) $n > $link->partCount()) {
            throw CloudError::invalid('número de parte fuera de rango');
        }
        $n = (int) $n;
        $expected = $link->expectedPartSize($n);
        $spool = $request->attributes->get('spool');
        if ((string) $request->headers->get('Content-Length') !== (string) $expected || $spool === null || $spool['size'] !== $expected) {
            throw CloudError::invalid('la parte no mide lo que le corresponde');
        }

        PartStore::put($link, $n, $spool['path']);
        $request->attributes->remove('spool');
        $sha = (string) $request->headers->get('X-Capataz-Content-SHA256'); // ya verificado contra el cuerpo
        LinkPart::upsert(
            [['link_id' => $link->id, 'part_number' => $n, 'size' => $expected, 'sha256' => $sha, 'created_at' => now(), 'updated_at' => now()]],
            ['link_id', 'part_number'],
            ['size', 'sha256', 'updated_at'],
        );
        $link->last_activity_at = now(); // C5: la actividad cuenta desde la última parte
        $link->save();

        return response()->json(['partNumber' => $n, 'size' => $expected, 'sha256' => $sha], 200, ['Cache-Control' => 'no-store']);
    }

    /** 410 si ya no está disponible; 409 si no admite partes (activo, fallido). */
    public static function requireUploading(Link $link): void
    {
        match ($link->state) {
            Link::UPLOADING => null,
            Link::EXPIRED => throw new CloudError(410, 'link_expired', 'el enlace venció'),
            Link::REVOKED => throw new CloudError(410, 'link_revoked', 'el enlace fue revocado'),
            Link::EXHAUSTED => throw new CloudError(410, 'downloads_exhausted', 'el enlace agotó sus descargas'),
            default => throw CloudError::conflict('conflict', 'el enlace no admite partes en su estado actual'),
        };
    }
}
