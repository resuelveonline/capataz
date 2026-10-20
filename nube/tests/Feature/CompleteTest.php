<?php

namespace Tests\Feature;

use App\Models\Link;
use App\Support\PartStore;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class CompleteTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    protected function setUp(): void
    {
        parent::setUp();
        $this->makeInstance();
        config(['capataz.limits.min_part_size' => 16]);
    }

    public function test_complete_activates_and_is_idempotent(): void
    {
        [$body, $pkg] = $this->activeLink(100, 16); // 7 partes, la última de 4
        $id = $body['linkId'];
        $sha = hash('sha256', $pkg);
        $link = Link::first();
        $this->assertSame(Link::ACTIVE, $link->state);
        $this->assertNotNull($link->activated_at);
        $this->assertSame($sha, $link->package_sha256);
        // Partes sin ensamblar: siguen siendo 7 archivos.
        $this->assertCount(7, glob(PartStore::dir($link).'/p*.bin'));

        $this->complete($id, $sha)->assertOk()->assertJson(['state' => 'active']);
        $this->complete($id, str_repeat('0', 64))->assertStatus(409)->assertJson(['code' => 'conflict']);
        $this->assertSame(Link::ACTIVE, Link::first()->state);
        // Ya activo: no admite más partes.
        $this->putPart($id, 1, random_bytes(16))->assertStatus(409);
    }

    public function test_missing_parts(): void
    {
        $body = $this->openBody(['encryptedSize' => 64, 'partSize' => 16]);
        $this->open($body)->assertCreated();
        $this->putPart($body['linkId'], 2, random_bytes(16))->assertOk();
        $this->complete($body['linkId'], str_repeat('a', 64))->assertStatus(409)
            ->assertJson(['code' => 'parts_missing', 'missingParts' => [1, 3, 4]]);
        // Una parte registrada cuyo archivo desapareció también falta.
        unlink(PartStore::partPath(Link::first(), 2));
        $this->complete($body['linkId'], str_repeat('a', 64))->assertJson(['missingParts' => [1, 2, 3, 4]]);
        $this->assertSame(Link::UPLOADING, Link::first()->state);
    }

    public function test_checksum_mismatch_fails_and_purges(): void
    {
        $body = $this->openBody(['encryptedSize' => 48, 'partSize' => 16]);
        $this->open($body)->assertCreated();
        foreach ([1, 2, 3] as $n) {
            $this->putPart($body['linkId'], $n, random_bytes(16))->assertOk();
        }
        $this->complete($body['linkId'], str_repeat('b', 64))->assertStatus(422)->assertJson(['code' => 'checksum_mismatch']);
        $link = Link::first();
        $this->assertSame([Link::FAILED, 'checksum_mismatch'], [$link->state, $link->failure_code]);
        $this->assertDirectoryDoesNotExist(PartStore::dir($link));
        $this->assertSame(0, $link->parts()->count());
        $this->signed('GET', '/v1/links/'.$body['linkId'])->assertJson(['state' => 'failed', 'failureCode' => 'checksum_mismatch']);
        $this->complete($body['linkId'], str_repeat('b', 64))->assertStatus(409);
    }

    public function test_tampered_part_on_disk_is_detected(): void
    {
        $body = $this->openBody(['encryptedSize' => 48, 'partSize' => 16]);
        $this->open($body)->assertCreated();
        $parts = [random_bytes(16), random_bytes(16), random_bytes(16)];
        foreach ($parts as $i => $p) {
            $this->putPart($body['linkId'], $i + 1, $p)->assertOk();
        }
        file_put_contents(PartStore::partPath(Link::first(), 2), random_bytes(15)); // tamaño alterado
        $this->complete($body['linkId'], hash('sha256', implode('', $parts)))->assertStatus(422);
    }

    public function test_bad_requests(): void
    {
        $body = $this->openBody(['encryptedSize' => 48, 'partSize' => 16]);
        $this->open($body)->assertCreated();
        foreach (['{}', '{"ciphertextSha256":"ABC"}', '{"ciphertextSha256":"'.str_repeat('a', 64).'","x":1}', 'nada'] as $b) {
            $this->signed('POST', '/v1/links/'.$body['linkId'].'/complete', $b)->assertStatus(400);
        }
        $this->complete('no-existe-00001', str_repeat('a', 64))->assertNotFound();
        Link::query()->update(['state' => Link::REVOKED]);
        $this->complete($body['linkId'], str_repeat('a', 64))->assertStatus(410)->assertJson(['code' => 'link_revoked']);
    }
}
