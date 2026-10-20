<?php

namespace Tests\Feature;

use App\Models\Link;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class PublicTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    protected function setUp(): void
    {
        parent::setUp();
        $this->makeInstance();
        config(['capataz.limits.min_part_size' => 16]);
    }

    private function token(array $body): string
    {
        return $this->tokens[$body['linkId']];
    }

    public function test_meta_and_full_blob(): void
    {
        [$body, $pkg] = $this->activeLink(100, 16, ['maxDownloads' => 5]);
        $t = $this->token($body);

        $meta = $this->get("/s/$t/meta")->assertOk()
            ->assertHeader('Referrer-Policy', 'no-referrer')->assertHeader('X-Robots-Tag', 'noindex, nofollow');
        $meta->assertExactJson(['state' => 'active', 'expiresAt' => $body['expiresAt'], 'encryptedSize' => 100,
            'cipherVersion' => 'cpk1', 'metaBlob' => $body['metaBlob'], 'downloadsRemaining' => 5]);
        $this->assertStringContainsString('no-store', $meta->headers->get('Cache-Control'));

        $res = $this->get("/s/$t/blob")->assertOk();
        $this->assertSame($pkg, $res->streamedContent());
        $this->assertSame('application/octet-stream', $res->headers->get('Content-Type'));
        $this->assertSame('bytes', $res->headers->get('Accept-Ranges'));
        $this->assertSame('100', $res->headers->get('Content-Length'));
        $this->assertNull($res->headers->get('Content-Disposition'));
        $this->assertStringContainsString('no-store', $res->headers->get('Cache-Control'));
        $this->assertSame(1, Link::first()->downloads);
        $this->get("/s/$t/meta")->assertJson(['downloadsRemaining' => 4]);
    }

    public function test_states(): void
    {
        $b = $this->openBody(['encryptedSize' => 64, 'partSize' => 16]);
        $this->open($b)->assertCreated();
        $t = $this->token($b);
        foreach (['meta', 'blob'] as $p) {
            $this->get("/s/$t/$p")->assertStatus(409)->assertJson(['code' => 'link_not_ready']);
        }
        foreach ([Link::FAILED => [404, 'not_found'], Link::EXPIRED => [410, 'link_expired'],
            Link::REVOKED => [410, 'link_revoked']] as $state => [$status, $code]) {
            Link::query()->update(['state' => $state]);
            foreach (['meta', 'blob'] as $p) {
                $this->get("/s/$t/$p")->assertStatus($status)->assertJson(['code' => $code])->assertHeader('Referrer-Policy', 'no-referrer');
            }
        }
        $this->get('/s/'.str_repeat('A', 43).'/meta')->assertNotFound();
        $this->get('/s/corto/blob')->assertNotFound();
    }

    public function test_download_cap_and_exhaustion(): void
    {
        [$body, $pkg] = $this->activeLink(64, 16, ['maxDownloads' => 2]);
        $t = $this->token($body);
        // HEAD no cuenta.
        $this->call('HEAD', "/s/$t/blob")->assertOk()->assertHeader('Content-Length', '64');
        $this->assertSame(0, Link::first()->downloads);

        $this->assertSame($pkg, $this->get("/s/$t/blob")->assertOk()->streamedContent());
        $this->assertSame($pkg, $this->get("/s/$t/blob")->assertOk()->streamedContent()); // la segunda agota
        $link = Link::first();
        $this->assertSame([2, Link::EXHAUSTED], [$link->downloads, $link->state]);
        $this->assertNotNull($link->exhausted_at);

        $this->get("/s/$t/blob")->assertStatus(410)->assertJson(['code' => 'downloads_exhausted']);
        $this->call('HEAD', "/s/$t/blob")->assertStatus(410);
        $this->get("/s/$t/meta")->assertStatus(410)->assertJson(['code' => 'downloads_exhausted']);
        $this->assertSame(2, Link::first()->downloads);
    }

    public function test_exhausted_bytes_are_kept_for_the_grace_then_purged(): void
    {
        [$body] = $this->activeLink(64, 16, ['maxDownloads' => 1]);
        $this->get('/s/'.$this->token($body).'/blob')->assertOk()->streamedContent();
        $this->travel(59)->minutes();
        $this->get('/s/'.$this->token($body).'/meta')->assertStatus(410);
        $this->assertNull(Link::first()->purged_at);
        $this->travel(2)->minutes();
        $this->get('/s/'.$this->token($body).'/meta')->assertStatus(410);
        $this->assertNotNull(Link::first()->purged_at);
        $this->assertSame(Link::EXHAUSTED, Link::first()->state);
    }

    public function test_unlimited_downloads_count(): void
    {
        [$body] = $this->activeLink(48, 16);
        foreach (range(1, 3) as $i) {
            $this->get('/s/'.$this->token($body).'/blob')->assertOk()->streamedContent();
        }
        $this->assertSame(3, Link::first()->downloads);
        $this->get('/s/'.$this->token($body).'/meta')->assertJson(['downloadsRemaining' => null]);
    }

    public function test_page_is_the_same_for_any_token(): void
    {
        $a = $this->get('/s/'.str_repeat('A', 43))->assertOk();
        $b = $this->get('/s/cualquier-cosa')->assertOk();
        $this->assertSame($a->getContent(), $b->getContent());
        $csp = $a->headers->get('Content-Security-Policy');
        $this->assertStringContainsString("default-src 'none'", $csp);
        $this->assertStringContainsString("frame-ancestors 'none'", $csp);
        $a->assertHeader('Referrer-Policy', 'no-referrer')->assertHeader('X-Robots-Tag', 'noindex, nofollow');
        $this->assertStringNotContainsString('<script>', $a->getContent());
    }
}
