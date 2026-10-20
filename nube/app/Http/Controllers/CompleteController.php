<?php

namespace App\Http\Controllers;

use App\Models\Link;
use App\Services\Lifecycle;
use App\Support\CloudError;
use App\Support\CloudLinkView;
use App\Support\PartStore;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

class CompleteController
{
    /**
     * POST /v1/links/{linkId}/complete: comprueba que estén todas las
     * partes con su tamaño, que sumen encryptedSize y que el SHA-256 del
     * paquete (leyendo las partes en orden, sin ensamblarlo ni cargarlo en
     * memoria) sea ciphertextSha256. Si cuadra, el enlace pasa a active.
     */
    public function complete(Request $request, string $linkId): JsonResponse
    {
        $link = Lifecycle::refresh(LinksController::findFor($request, $linkId));
        $want = $this->parse($request->getContent());

        if ($link->state === Link::ACTIVE) {
            // Idempotente con el mismo hash; con otro, conflict (C8).
            if ($link->package_sha256 !== null && hash_equals($link->package_sha256, $want)) {
                return $this->ok($link);
            }
            throw CloudError::conflict();
        }
        PartsController::requireUploading($link);

        $count = $link->partCount();
        $have = $link->parts()->pluck('size', 'part_number')->mapWithKeys(fn ($s, $n) => [(int) $n => (int) $s])->all();
        $missing = [];
        for ($n = 1; $n <= $count; $n++) {
            if (! isset($have[$n]) || ! is_file(PartStore::partPath($link, $n))) {
                $missing[] = $n;
            }
        }
        if ($missing !== []) {
            throw CloudError::conflict('parts_missing', 'faltan partes', ['missingParts' => $missing]);
        }

        $actual = $this->hashParts($link, $count);
        if ($actual === null || ! hash_equals($actual, $want)) {
            $link->state = Link::FAILED;
            $link->failure_code = 'checksum_mismatch';
            Lifecycle::purge($link);
            $link->save();
            throw new CloudError(422, 'checksum_mismatch', 'el paquete ensamblado no coincide');
        }

        $link->state = Link::ACTIVE;
        $link->activated_at = now();
        $link->package_sha256 = $actual;
        $link->save();

        return $this->ok($link);
    }

    /** SHA-256 hex de las partes en orden, o null si alguna no mide lo que debe. */
    private function hashParts(Link $link, int $count): ?string
    {
        @set_time_limit(300);
        $ctx = hash_init('sha256');
        $total = 0;
        for ($n = 1; $n <= $count; $n++) {
            $path = PartStore::partPath($link, $n);
            $size = @filesize($path);
            if ($size !== $link->expectedPartSize($n)) {
                return null;
            }
            $fh = fopen($path, 'rb');
            $read = hash_update_stream($ctx, $fh);
            fclose($fh);
            if ($read !== $size) {
                return null;
            }
            $total += $size;
        }

        return $total === $link->encrypted_size ? hash_final($ctx) : null;
    }

    private function parse(string $body): string
    {
        $d = json_decode($body, true, 4);
        if (! is_array($d) || array_keys($d) !== ['ciphertextSha256'] || ! is_string($d['ciphertextSha256'])
            || ! preg_match('/^[0-9a-f]{64}$/', $d['ciphertextSha256'])) {
            throw CloudError::invalid('CompleteRequest inválido');
        }

        return $d['ciphertextSha256'];
    }

    private function ok(Link $link): JsonResponse
    {
        return response()->json(CloudLinkView::of($link), 200, ['Cache-Control' => 'no-store']);
    }
}
