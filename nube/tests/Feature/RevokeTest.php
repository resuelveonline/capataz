<?php

namespace Tests\Feature;

use App\Models\Link;
use App\Support\PartStore;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class RevokeTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    protected function setUp(): void
    {
        parent::setUp();
        $this->makeInstance();
        config(['capataz.limits.min_part_size' => 16]);
    }

    public function test_revoke_active_deletes_bytes_and_is_idempotent(): void
    {
        [$body] = $this->activeLink(64, 16);
        $id = $body['linkId'];
        $dir = PartStore::dir(Link::first());
        $this->assertDirectoryExists($dir);

        $this->signed('DELETE', "/v1/links/$id")->assertNoContent();
        $link = Link::first();
        $this->assertSame(Link::REVOKED, $link->state);
        $this->assertNotNull($link->purged_at);
        $this->assertDirectoryDoesNotExist($dir);
        $this->assertSame(0, $link->parts()->count());

        $this->signed('DELETE', "/v1/links/$id")->assertNoContent();
        $this->signed('GET', "/v1/links/$id")->assertJson(['state' => 'revoked']);
        // Revocado: ya no admite partes.
        $this->putPart($id, 1, random_bytes(16))->assertStatus(410)->assertJson(['code' => 'link_revoked']);
    }

    public function test_revoke_uploading_and_terminal_states(): void
    {
        $b = $this->openBody(['encryptedSize' => 64, 'partSize' => 16]);
        $this->open($b)->assertCreated();
        $this->putPart($b['linkId'], 1, random_bytes(16))->assertOk();
        $this->signed('DELETE', '/v1/links/'.$b['linkId'])->assertNoContent();
        $this->assertSame(Link::REVOKED, Link::first()->state);

        foreach ([Link::EXPIRED, Link::FAILED] as $state) {
            Link::query()->update(['state' => $state]);
            $this->signed('DELETE', '/v1/links/'.$b['linkId'])->assertNoContent();
            $this->assertSame($state, Link::first()->state, 'no cambia un estado terminal');
        }
    }

    public function test_unknown_or_foreign_link_is_404(): void
    {
        $b = $this->openBody();
        $this->open($b)->assertCreated();
        $this->signed('DELETE', '/v1/links/nunca-existio-01')->assertNotFound()->assertJson(['code' => 'not_found']);
        $this->makeInstance('otra-instancia-0002');
        $this->signed('DELETE', '/v1/links/'.$b['linkId'])->assertNotFound();
        $this->assertSame(Link::UPLOADING, Link::first()->state);
    }
}
