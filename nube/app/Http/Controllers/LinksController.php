<?php

namespace App\Http\Controllers;

use App\Models\Instance;
use App\Models\Link;
use App\Services\Lifecycle;
use App\Services\Quota;
use App\Support\CloudError;
use App\Support\CloudLinkView;
use App\Support\Signature;
use Illuminate\Database\UniqueConstraintViolationException;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Carbon;
use Illuminate\Support\Facades\DB;

class LinksController
{
    private const FIELDS = ['linkId', 'tokenHash', 'expiresAt', 'maxDownloads', 'cipherVersion', 'encryptedSize', 'partSize', 'metaBlob'];

    /**
     * POST /v1/links: abre la subida. Idempotente por linkId: el mismo
     * cuerpo (comparado normalizado) devuelve 200 con el estado actual,
     * incluidas las partes recibidas; otro cuerpo, 409 conflict. La ventana
     * de vigencia solo se valida al crear, no al reabrir.
     */
    public function open(Request $request): JsonResponse
    {
        $instance = $this->instance($request);
        $req = $this->parseOpen($request->getContent());
        $hash = hash('sha256', json_encode($req));

        if ($existing = Link::where('link_id', $req['linkId'])->first()) {
            return $this->reopen($instance, $existing, $hash);
        }

        $now = now();
        $limits = config('capataz.limits');
        $tol = config('capataz.ttl_tolerance');
        $ttl = $req['expiresAt'] - $now->getTimestamp();
        if ($ttl < $limits['min_ttl_seconds'] - $tol || $ttl > $limits['max_ttl_seconds'] + $tol) {
            throw CloudError::invalid('expiresAt fuera de la vigencia permitida');
        }
        if ($req['partSize'] < $limits['min_part_size'] || $req['partSize'] > $limits['max_part_size']) {
            throw CloudError::invalid('partSize fuera de rango');
        }
        if ($req['encryptedSize'] > $limits['max_encrypted_bytes']) {
            throw new CloudError(413, 'payload_too_large', 'el paquete supera el tope');
        }

        try {
            $link = DB::transaction(function () use ($instance, $req, $hash, $now, $limits) {
                $usage = Quota::usage($instance);
                if ($usage['activeLinks'] >= $limits['max_active_links']
                    || $usage['storedBytes'] + $req['encryptedSize'] > $limits['max_stored_bytes']) {
                    throw new CloudError(413, 'quota_exceeded', 'cupo de la instancia agotado');
                }
                if (Link::where('token_hash', $req['tokenHash'])->exists()) {
                    throw CloudError::conflict();
                }

                return Link::create([
                    'link_id' => $req['linkId'], 'instance_id' => $instance->id, 'token_hash' => $req['tokenHash'],
                    'state' => Link::UPLOADING, 'expires_at' => Carbon::createFromTimestampUTC($req['expiresAt']),
                    'max_downloads' => $req['maxDownloads'], 'cipher_version' => $req['cipherVersion'],
                    'encrypted_size' => $req['encryptedSize'], 'part_size' => $req['partSize'],
                    'meta_blob' => $req['metaBlob'], 'request_hash' => $hash, 'last_activity_at' => $now,
                ]);
            });
        } catch (UniqueConstraintViolationException) {
            // Otra petición creó el mismo linkId o tokenHash a la vez.
            $existing = Link::where('link_id', $req['linkId'])->first();
            if ($existing !== null) {
                return $this->reopen($instance, $existing, $hash);
            }
            throw CloudError::conflict();
        }

        return response()->json(CloudLinkView::of($link), 201, ['Cache-Control' => 'no-store']);
    }

    private function reopen(Instance $instance, Link $existing, string $hash): JsonResponse
    {
        if ($existing->instance_id !== $instance->id || ! hash_equals($existing->request_hash, $hash)) {
            throw CloudError::conflict();
        }
        if ($existing->state === Link::UPLOADING) {
            $existing->last_activity_at = now(); // reabrir es actividad (C5)
            $existing->save();
        }

        return response()->json(CloudLinkView::of(Lifecycle::refresh($existing)), 200, ['Cache-Control' => 'no-store']);
    }

    /** GET /v1/links: del más reciente al más antiguo, con cursor opaco. */
    public function index(Request $request): JsonResponse
    {
        $instance = $this->instance($request);
        $state = $request->query('state');
        if ($state !== null && ! in_array($state, Link::STATES, true)) {
            throw CloudError::invalid('state inválido');
        }
        $limit = $request->query('limit', '50');
        if (! is_string($limit) || ! ctype_digit($limit) || (int) $limit < 1 || (int) $limit > 200) {
            throw CloudError::invalid('limit inválido');
        }
        $limit = (int) $limit;
        $cursor = $request->query('cursor');
        $before = null;
        if ($cursor !== null) {
            $raw = is_string($cursor) ? Signature::b64urlDecode($cursor) : false;
            if ($raw === false || ! preg_match('/^c(\d{1,19})$/', $raw, $m)) {
                throw CloudError::invalid('cursor inválido');
            }
            $before = (int) $m[1];
        }

        Lifecycle::refreshDue($instance->id);
        $q = Link::where('instance_id', $instance->id)->orderByDesc('id');
        if ($state !== null) {
            $q->where('state', $state);
        }
        if ($before !== null) {
            $q->where('id', '<', $before);
        }
        $rows = $q->limit($limit + 1)->get();
        $next = null;
        if ($rows->count() > $limit) {
            $rows = $rows->take($limit);
            $next = Signature::b64url('c'.$rows->last()->id);
        }

        return response()->json([
            'items' => $rows->map(fn (Link $l) => CloudLinkView::of($l))->values()->all(),
            'nextCursor' => $next,
        ], 200, ['Cache-Control' => 'no-store']);
    }

    /** GET /v1/links/{linkId}. */
    public function show(Request $request, string $linkId): JsonResponse
    {
        return response()->json(CloudLinkView::of(Lifecycle::refresh($this->find($request, $linkId))), 200, ['Cache-Control' => 'no-store']);
    }

    /** Enlace de la instancia que firma; el de otra instancia es 404. */
    public static function findFor(Request $request, string $linkId): Link
    {
        $instance = $request->attributes->get('instance');
        $link = preg_match('/^[A-Za-z0-9_-]{8,64}$/', $linkId)
            ? Link::where('link_id', $linkId)->where('instance_id', $instance->id)->first()
            : null;

        return $link ?? throw CloudError::notFound();
    }

    private function find(Request $request, string $linkId): Link
    {
        return self::findFor($request, $linkId);
    }

    private function instance(Request $request): Instance
    {
        return $request->attributes->get('instance');
    }

    /**
     * Valida OpenUploadRequest y lo normaliza (expiresAt en segundos Unix)
     * para comparar aperturas repetidas.
     *
     * @return array<string, mixed>
     */
    private function parseOpen(string $body): array
    {
        try {
            $d = json_decode($body, true, 8, JSON_THROW_ON_ERROR | JSON_BIGINT_AS_STRING);
        } catch (\JsonException) {
            throw CloudError::invalid('JSON inválido');
        }
        if (! is_array($d) || array_is_list($d) || array_diff(array_keys($d), self::FIELDS) !== []) {
            throw CloudError::invalid('campos inesperados');
        }
        $id = $d['linkId'] ?? null;
        $th = $d['tokenHash'] ?? null;
        $exp = $d['expiresAt'] ?? null;
        $max = $d['maxDownloads'] ?? null;
        $size = $d['encryptedSize'] ?? null;
        $part = $d['partSize'] ?? null;
        $blob = $d['metaBlob'] ?? null;
        $ok = is_string($id) && preg_match('/^[A-Za-z0-9_-]{8,64}$/', $id)
            && is_string($th) && preg_match('/^[A-Za-z0-9_-]{43}$/', $th)
            && is_string($exp) && preg_match('/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$/', $exp)
            && ($d['cipherVersion'] ?? null) === 'cpk1'
            && is_int($size) && $size >= 48 && is_int($part) && $part >= 1
            && ($max === null || (is_int($max) && $max >= 1))
            && is_string($blob) && $blob !== '' && strlen($blob) <= 262144;
        if (! $ok) {
            throw CloudError::invalid('OpenUploadRequest inválido');
        }
        $decoded = Signature::b64urlDecode($blob);
        if ($decoded === false || strlen($decoded) > config('capataz.limits.max_meta_blob_bytes')) {
            throw CloudError::invalid('metaBlob inválido');
        }
        try {
            $expTs = (new \DateTimeImmutable($exp))->getTimestamp();
        } catch (\Exception) {
            throw CloudError::invalid('expiresAt inválido');
        }

        return [
            'linkId' => $id, 'tokenHash' => $th, 'expiresAt' => $expTs, 'maxDownloads' => $max,
            'cipherVersion' => 'cpk1', 'encryptedSize' => $size, 'partSize' => $part, 'metaBlob' => $blob,
        ];
    }
}
