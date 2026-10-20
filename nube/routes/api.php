<?php

// Rutas de Capataz Nube (api/capataz-nube-api.yaml v0.1.1). /v1: API de
// instancia, firmada con HMAC. /s: descarga pública.

use App\Http\Controllers\InstanceController;
use Illuminate\Support\Facades\Route;

Route::middleware('capataz.signed')->prefix('v1')->group(function () {
    Route::get('instance', [InstanceController::class, 'show']);
});
