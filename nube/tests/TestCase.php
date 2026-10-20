<?php

namespace Tests;

use Illuminate\Foundation\Testing\TestCase as BaseTestCase;
use Illuminate\Support\Facades\File;

abstract class TestCase extends BaseTestCase
{
    protected string $dataDir;

    protected function setUp(): void
    {
        parent::setUp();
        // Cada prueba guarda los paquetes en un directorio propio.
        $this->dataDir = sys_get_temp_dir().'/capataz-nube-test-'.bin2hex(random_bytes(6));
        config(['capataz.data_dir' => $this->dataDir]);
    }

    protected function tearDown(): void
    {
        File::deleteDirectory($this->dataDir);
        parent::tearDown();
    }

    /** Ruta de un archivo de docs/ del monorepo. */
    protected function docsPath(string $file): string
    {
        return dirname(base_path()).'/docs/'.$file;
    }
}
