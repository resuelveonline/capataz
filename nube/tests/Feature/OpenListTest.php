<?php

namespace Tests\Feature;

use App\Models\Link;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Illuminate\Support\Carbon;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class OpenListTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    protected function setUp(): void
    {
        parent::setUp();
        Carbon::setTestNow('2026-10-19 12:00:00');
        $this->makeInstance();
    }

    public function test_open_creates_and_is_idempotent(): void
    {
        $body = $this->openBody(['maxDownloads' => 3]);
        $res = $this->open($body)->assertCreated();
        $res->assertExactJson([
            'linkId' => $body['linkId'], 'state' => 'uploading', 'createdAt' => '2026-10-19T12:00:00Z',
            'activatedAt' => null, 'expiresAt' => '2026-10-20T12:00:00Z', 'encryptedSize' => 3 * 1048576 + 100,
            'partSize' => 1048576, 'partCount' => 4, 'receivedParts' => [], 'downloads' => 0,
            'maxDownloads' => 3, 'failureCode' => null,
        ]);
        $this->assertStringNotContainsString($body['tokenHash'], $res->getContent());

        // Mismo cuerpo, aunque cambie el formato de la fecha: 200.
        $same = $body;
        $same['expiresAt'] = '2026-10-20T06:00:00-06:00';
        $this->open($same)->assertOk()->assertJson(['state' => 'uploading']);
        // Reabrir horas después no vuelve a validar la vigencia.
        Carbon::setTestNow('2026-10-20 11:59:00');
        $this->open($body)->assertOk();

        // Otro cuerpo con el mismo linkId: conflict.
        $this->open(['maxDownloads' => 4] + $body)->assertStatus(409)->assertJson(['code' => 'conflict']);
        $this->assertSame(1, Link::count());
    }

    public function test_open_validation(): void
    {
        $cases = [
            'sin linkId' => fn ($b) => array_diff_key($b, ['linkId' => 1]),
            'linkId corto' => fn ($b) => ['linkId' => 'corto'] + $b,
            'linkId con barra' => fn ($b) => ['linkId' => 'enlace/barra01'] + $b,
            'tokenHash corto' => fn ($b) => ['tokenHash' => 'abc'] + $b,
            'otro cipher' => fn ($b) => ['cipherVersion' => 'cpk2'] + $b,
            'tamaño como texto' => fn ($b) => ['encryptedSize' => '1000'] + $b,
            'tamaño menor de 48' => fn ($b) => ['encryptedSize' => 47] + $b,
            'maxDownloads 0' => fn ($b) => ['maxDownloads' => 0] + $b,
            'metaBlob no base64url' => fn ($b) => ['metaBlob' => 'no+es/base64'] + $b,
            'campo extra' => fn ($b) => $b + ['extra' => 1],
            'fecha inválida' => fn ($b) => ['expiresAt' => 'mañana'] + $b,
            'vigencia corta' => fn ($b) => ['expiresAt' => '2026-10-19T12:54:59Z'] + $b,
            'vigencia larga' => fn ($b) => ['expiresAt' => '2026-11-18T12:05:01Z'] + $b,
            'parte chica' => fn ($b) => ['partSize' => 1048575] + $b,
            'parte grande' => fn ($b) => ['partSize' => 8388609] + $b,
        ];
        foreach ($cases as $name => $mod) {
            $this->open($mod($this->openBody()))->assertStatus(400)->assertJson(['code' => 'invalid_request'], $name);
        }
        // Dentro de la tolerancia de 300 s: aceptado.
        $this->open(['expiresAt' => '2026-10-19T12:55:00Z'] + $this->openBody())->assertCreated();
        $this->signed('POST', '/v1/links', '[1,2]')->assertStatus(400);
        $this->signed('POST', '/v1/links', '{')->assertStatus(400);
    }

    public function test_limits_and_quota(): void
    {
        $this->open($this->openBody(['encryptedSize' => 524296033]))->assertStatus(413)->assertJson(['code' => 'payload_too_large']);

        config(['capataz.limits.max_active_links' => 2]);
        $this->open($this->openBody())->assertCreated();
        $this->open($this->openBody())->assertCreated();
        $this->open($this->openBody())->assertStatus(413)->assertJson(['code' => 'quota_exceeded']);

        config(['capataz.limits.max_active_links' => 100, 'capataz.limits.max_stored_bytes' => 3 * (3 * 1048576 + 100) - 1]);
        $this->open($this->openBody())->assertStatus(413)->assertJson(['code' => 'quota_exceeded']);
    }

    public function test_token_hash_must_be_unique(): void
    {
        $a = $this->openBody();
        $this->open($a)->assertCreated();
        $this->open($this->openBody(['tokenHash' => $a['tokenHash']]))->assertStatus(409)->assertJson(['code' => 'conflict']);
    }

    public function test_links_are_scoped_to_the_instance(): void
    {
        $mine = $this->openBody();
        $this->open($mine)->assertCreated();
        $key = $this->instanceKey;
        $first = $this->instance;

        $this->makeInstance('otra-instancia-0001');
        $this->signed('GET', '/v1/links/'.$mine['linkId'])->assertNotFound();
        $this->open($mine)->assertStatus(409); // mismo linkId, otra instancia
        $this->signed('GET', '/v1/links')->assertJson(['items' => []]);

        $this->signed('GET', '/v1/links/'.$mine['linkId'], '', $first, $key)->assertOk();
    }

    public function test_list_pagination_and_filter(): void
    {
        $ids = [];
        foreach (range(1, 5) as $i) {
            $b = $this->openBody();
            $this->open($b)->assertCreated();
            $ids[] = $b['linkId'];
        }
        Link::where('link_id', $ids[1])->update(['state' => Link::REVOKED]);

        $p1 = $this->signed('GET', '/v1/links?limit=2')->assertOk();
        $this->assertSame([$ids[4], $ids[3]], array_column($p1->json('items'), 'linkId'));
        $cursor = $p1->json('nextCursor');
        $this->assertNotNull($cursor);
        $p2 = $this->signed('GET', '/v1/links?limit=2&cursor='.$cursor)->assertOk();
        $this->assertSame([$ids[2], $ids[1]], array_column($p2->json('items'), 'linkId'));
        $p3 = $this->signed('GET', '/v1/links?limit=2&cursor='.$p2->json('nextCursor'))->assertOk();
        $this->assertSame([$ids[0]], array_column($p3->json('items'), 'linkId'));
        $this->assertNull($p3->json('nextCursor'));

        $this->assertSame([$ids[1]], array_column($this->signed('GET', '/v1/links?state=revoked')->json('items'), 'linkId'));
        foreach (['state=otro', 'limit=0', 'limit=201', 'limit=x', 'cursor=nada'] as $q) {
            $this->signed('GET', '/v1/links?'.$q)->assertStatus(400)->assertJson(['code' => 'invalid_request']);
        }
    }

    public function test_show_applies_expiry(): void
    {
        $b = $this->openBody(['expiresAt' => '2026-10-19T13:00:00Z']);
        $this->open($b)->assertCreated();
        $this->signed('GET', '/v1/links/'.$b['linkId'])->assertOk()->assertJson(['state' => 'uploading']);
        Carbon::setTestNow('2026-10-19 13:00:00');
        $this->signed('GET', '/v1/links/'.$b['linkId'])->assertOk()->assertJson(['state' => 'expired']);
        $this->signed('GET', '/v1/links/no-existe-0000')->assertNotFound()->assertJson(['code' => 'not_found']);
    }
}
