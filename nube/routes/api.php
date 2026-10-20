<?php

// Rutas de Capataz Nube (api/capataz-nube-api.yaml v0.1.1). /v1: API de
// instancia, firmada con HMAC. /s: descarga pública.

use App\Http\Controllers\CompleteController;
use App\Http\Controllers\InstanceController;
use App\Http\Controllers\LinksController;
use App\Http\Controllers\PartsController;
use App\Http\Controllers\PublicController;
use Illuminate\Support\Facades\Route;

Route::middleware('capataz.signed')->prefix('v1')->group(function () {
    Route::get('instance', [InstanceController::class, 'show']);
    Route::post('links', [LinksController::class, 'open']);
    Route::get('links', [LinksController::class, 'index']);
    Route::get('links/{linkId}', [LinksController::class, 'show']);
    Route::delete('links/{linkId}', [LinksController::class, 'revoke']);
    Route::put('links/{linkId}/parts/{n}', [PartsController::class, 'put']);
    Route::post('links/{linkId}/complete', [CompleteController::class, 'complete']);
});

Route::get('s/{token}', [PublicController::class, 'page']);
Route::get('s/{token}/meta', [PublicController::class, 'meta']);
Route::get('s/{token}/blob', [PublicController::class, 'blob']);
