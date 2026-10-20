<?php

namespace App\Console\Commands;

use App\Models\Instance;
use Illuminate\Console\Command;

/**
 * Da de alta una instancia de Capataz y muestra su clave HMAC una sola vez.
 *
 * La clave sale sola en stdout (para redirigirla a un archivo 0600:
 * `php artisan capataz:instance:add ID > clave`); los avisos van a stderr.
 * Se guarda cifrada con APP_KEY y no se puede volver a mostrar.
 */
class InstanceAdd extends Command
{
    protected $signature = 'capataz:instance:add {instanceId : el instanceId de Capataz (8 a 64 caracteres [A-Za-z0-9_-])}';

    protected $description = 'Da de alta una instancia y muestra su clave una sola vez';

    public function handle(): int
    {
        $id = (string) $this->argument('instanceId');
        $err = $this->output->getErrorStyle();
        if (! preg_match('/^[A-Za-z0-9_-]{8,64}$/', $id)) {
            $err->writeln('instanceId inválido: 8 a 64 caracteres [A-Za-z0-9_-]');

            return self::INVALID;
        }
        if (Instance::where('instance_id', $id)->exists()) {
            $err->writeln('Esa instancia ya existe; su clave no se puede volver a mostrar.');

            return self::FAILURE;
        }
        $key = rtrim(strtr(base64_encode(random_bytes(32)), '+/', '-_'), '=');
        Instance::create(['instance_id' => $id, 'key' => $key, 'active' => true]);

        $err->writeln('Instancia dada de alta. Guarda la clave en CAPATAZ_NUBE_KEY del host; no se volverá a mostrar:');
        $this->output->writeln($key);

        return self::SUCCESS;
    }
}
