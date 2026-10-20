<?php

namespace Tests\Feature;

use App\Models\Link;
use App\Support\PartStore;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class PartsTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    private array $body;

    protected function setUp(): void
    {
        parent::setUp();
        $this->makeInstance();
        config(['capataz.limits.min_part_size' => 16]);
        // 4 partes: 16, 16, 16 y 5 bytes (el contrato pide al menos 48).
        $this->body = $this->openBody(['encryptedSize' => 53, 'partSize' => 16]);
        $this->open($this->body)->assertCreated();
    }

    public function test_upload_replace_and_receipt(): void
    {
        $id = $this->body['linkId'];
        $p1 = random_bytes(16);
        $this->putPart($id, 1, $p1)->assertOk()->assertExactJson(['partNumber' => 1, 'size' => 16, 'sha256' => hash('sha256', $p1)]);
        $this->putPart($id, 4, 'abcde')->assertOk()->assertJson(['size' => 5]);
        $this->signed('GET', "/v1/links/$id")->assertJson(['receivedParts' => [1, 4]]);

        // Sustituir una parte.
        $p1b = random_bytes(16);
        $this->putPart($id, 1, $p1b)->assertOk()->assertJson(['sha256' => hash('sha256', $p1b)]);
        $link = Link::first();
        $this->assertSame($p1b, file_get_contents(PartStore::partPath($link, 1)));
        $this->assertSame(2, $link->parts()->count());
        $this->assertSame([], glob($this->dataDir.'/tmp/*') ?: [], 'sin temporales');
        $this->assertSame('0700', substr(sprintf('%o', fileperms(PartStore::dir($link))), -4));
    }

    public function test_size_and_number_rules(): void
    {
        $id = $this->body['linkId'];
        $this->putPart($id, 1, random_bytes(15))->assertStatus(400)->assertJson(['code' => 'invalid_request']);
        $this->putPart($id, 1, random_bytes(17))->assertStatus(400); // no es el tamaño de la parte
        config(['capataz.limits.max_part_size' => 16]);
        $this->putPart($id, 1, random_bytes(17))->assertStatus(413)->assertJson(['code' => 'payload_too_large']);
        config(['capataz.limits.max_part_size' => 8388608]);
        $this->putPart($id, 4, random_bytes(16))->assertStatus(400); // la última mide 5
        $this->putPart($id, 3, 'abcde')->assertStatus(400); // las demás miden 16
        $this->putPart($id, 2, '')->assertStatus(400);
        $this->putPart($id, 0, random_bytes(16))->assertStatus(400);
        $this->putPart($id, 5, random_bytes(5))->assertStatus(400);
        $this->signed('PUT', "/v1/links/$id/parts/x", random_bytes(16))->assertStatus(400);
        $this->putPart('no-existe-00001', 1, random_bytes(16))->assertNotFound();
        $this->assertSame(0, Link::first()->parts()->count());
        $this->assertSame([], glob($this->dataDir.'/tmp/*') ?: []);
    }

    public function test_states(): void
    {
        $id = $this->body['linkId'];
        foreach ([Link::EXPIRED => [410, 'link_expired'], Link::REVOKED => [410, 'link_revoked'],
            Link::ACTIVE => [409, 'conflict'], Link::FAILED => [409, 'conflict']] as $state => [$status, $code]) {
            Link::query()->update(['state' => $state]);
            $this->putPart($id, 1, random_bytes(16))->assertStatus($status)->assertJson(['code' => $code]);
        }
    }

    public function test_activity_is_tracked(): void
    {
        $this->travel(5)->hours();
        $this->putPart($this->body['linkId'], 2, random_bytes(16))->assertOk();
        $this->assertTrue(Link::first()->last_activity_at->equalTo(now()->startOfSecond()));
    }
}
