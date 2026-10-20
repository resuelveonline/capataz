<?php

namespace Tests\Unit;

use App\Support\Signature;
use PHPUnit\Framework\TestCase;

/** Reproduce docs/vectores-nube.json (generados con el cliente de Go). */
class SignatureVectorsTest extends TestCase
{
    private function vectors(): array
    {
        return json_decode(file_get_contents(dirname(__DIR__, 3).'/docs/vectores-nube.json'), true, flags: JSON_THROW_ON_ERROR);
    }

    public function test_token_hash_vectors(): void
    {
        $v = $this->vectors()['tokenHash'];
        $this->assertCount(3, $v);
        foreach ($v as $c) {
            $this->assertSame($c['tokenHash'], Signature::tokenHash($c['token']), $c['token']);
        }
    }

    public function test_signature_vectors(): void
    {
        $sig = $this->vectors()['signature'];
        $key = Signature::b64urlDecode($sig['instanceKeyBase64url']);
        $this->assertSame($sig['instanceKeyHex'], bin2hex($key));
        $this->assertCount(3, $sig['cases']);
        foreach ($sig['cases'] as $c) {
            $body = self::body($c);
            $hex = hash('sha256', $body);
            $canonical = Signature::canonical($c['method'], $c['target'], $c['timestamp'], $c['nonce'], $hex);
            $this->assertSame($c['contentSha256'], $hex, $c['name']);
            $this->assertSame($c['canonical'], $canonical, $c['name']);
            $this->assertSame($c['signature'], Signature::sign($key, $canonical), $c['name']);
        }
    }

    public static function body(array $c): string
    {
        return match ($c['bodyEncoding']) {
            'empty' => '',
            'utf8' => $c['body'],
            'hex' => hex2bin($c['body']),
        };
    }
}
