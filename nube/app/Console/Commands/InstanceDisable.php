<?php

namespace App\Console\Commands;

use App\Models\Instance;
use Illuminate\Console\Command;

/** Desactiva una instancia: sus peticiones firmadas pasan a unknown_instance. */
class InstanceDisable extends Command
{
    protected $signature = 'capataz:instance:disable {instanceId}';

    protected $description = 'Desactiva una instancia (deja de poder firmar)';

    public function handle(): int
    {
        $n = Instance::where('instance_id', (string) $this->argument('instanceId'))->update(['active' => false]);
        if ($n === 0) {
            $this->output->getErrorStyle()->writeln('No existe esa instancia.');

            return self::FAILURE;
        }
        $this->output->getErrorStyle()->writeln('Instancia desactivada.');

        return self::SUCCESS;
    }
}
