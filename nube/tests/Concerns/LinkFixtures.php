<?php

namespace Tests\Concerns;

use App\Support\Signature;
use Illuminate\Testing\TestResponse;

/** Aperturas, partes y paquetes de prueba (usa SignedRequests). */
trait LinkFixtures
{
    /** @var array<string, string> linkId => token */
    protected array $tokens = [];

    protected function openBody(array $over = []): array
    {
        $token = Signature::b64url(random_bytes(32));
        $body = array_merge([
            'linkId' => 'enlace-'.bin2hex(random_bytes(6)),
            'tokenHash' => Signature::tokenHash($token),
            'expiresAt' => now()->addDay()->utc()->format('Y-m-d\TH:i:s\Z'),
            'maxDownloads' => null,
            'cipherVersion' => 'cpk1',
            'encryptedSize' => 3 * 1048576 + 100,
            'partSize' => 1048576,
            'metaBlob' => Signature::b64url(random_bytes(64)),
        ], $over);
        $this->tokens[$body['linkId']] = $token;

        return $body;
    }

    protected function open(array $body): TestResponse
    {
        return $this->signedJson('POST', '/v1/links', $body);
    }

    protected function putPart(string $linkId, int $n, string $bytes): TestResponse
    {
        return $this->signed('PUT', "/v1/links/$linkId/parts/$n", $bytes);
    }

    protected function complete(string $linkId, string $sha256Hex): TestResponse
    {
        return $this->signedJson('POST', "/v1/links/$linkId/complete", ['ciphertextSha256' => $sha256Hex]);
    }

    /** Abre, sube y completa un paquete aleatorio. Devuelve [cuerpo, paquete]. */
    protected function activeLink(int $size, int $partSize, array $over = []): array
    {
        $body = $this->openBody(['encryptedSize' => $size, 'partSize' => $partSize] + $over);
        $this->open($body)->assertCreated();
        $pkg = random_bytes($size);
        foreach (str_split($pkg, $partSize) as $i => $part) {
            $this->putPart($body['linkId'], $i + 1, $part)->assertOk();
        }
        $this->complete($body['linkId'], hash('sha256', $pkg))->assertOk()->assertJson(['state' => 'active']);

        return [$body, $pkg];
    }
}
