<?php

namespace Tests\Feature;

use App\Models\Link;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class InstanceEndpointTest extends TestCase
{
    use RefreshDatabase, SignedRequests;

    public function test_limits_usage_and_clock(): void
    {
        $this->makeInstance();
        config(['capataz.limits.max_active_links' => 7]);
        $res = $this->signed('GET', '/v1/instance');
        $res->assertOk()->assertHeader('Cache-Control', 'no-store, private')->assertExactJson([
            'instanceId' => 'instancia-pruebas-0001',
            'serverTime' => now()->getTimestamp(),
            'limits' => [
                'maxEncryptedBytes' => 524296032, 'minPartSize' => 1048576, 'maxPartSize' => 8388608,
                'minTtlSeconds' => 3600, 'maxTtlSeconds' => 2592000, 'maxActiveLinks' => 7,
                'maxStoredBytes' => 5368709120,
            ],
            'usage' => ['activeLinks' => 0, 'storedBytes' => 0],
        ]);
    }

    public function test_usage_counts_uploading_and_active_only(): void
    {
        $this->makeInstance();
        $other = $this->instance;
        foreach ([[Link::UPLOADING, 100, '+1 hour'], [Link::ACTIVE, 200, '+1 hour'], [Link::ACTIVE, 400, '-1 second'],
            [Link::REVOKED, 800, '+1 hour'], [Link::FAILED, 1600, '+1 hour']] as $i => [$state, $size, $exp]) {
            Link::create([
                'link_id' => 'enlace-uso-000'.$i, 'instance_id' => $other->id, 'token_hash' => str_pad((string) $i, 43, 'a'),
                'state' => $state, 'expires_at' => now()->modify($exp), 'cipher_version' => 'cpk1',
                'encrypted_size' => $size, 'part_size' => 1048576, 'meta_blob' => 'AA', 'request_hash' => str_repeat('0', 64),
                'last_activity_at' => now(),
            ]);
        }
        $this->signed('GET', '/v1/instance')->assertJson(['usage' => ['activeLinks' => 2, 'storedBytes' => 300]]);
        $this->assertSame(Link::EXPIRED, Link::where('link_id', 'enlace-uso-0002')->value('state'));
    }

    public function test_requires_signature(): void
    {
        $this->getJson('/v1/instance')->assertStatus(401)->assertJson(['code' => 'unknown_instance']);
        $this->makeInstance();
        $this->postJson('/v1/instance')->assertStatus(405);
    }
}
