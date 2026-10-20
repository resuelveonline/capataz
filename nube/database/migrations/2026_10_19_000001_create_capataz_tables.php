<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

// Tablas de Capataz Nube. Todas las fechas en UTC.
return new class extends Migration
{
    public function up(): void
    {
        Schema::create('instances', function (Blueprint $t) {
            $t->id();
            $t->string('instance_id', 64)->unique();
            // Clave HMAC de 32 bytes en base64url, cifrada con APP_KEY.
            $t->text('key');
            $t->boolean('active')->default(true);
            $t->timestamps();
        });

        Schema::create('links', function (Blueprint $t) {
            // id ordena por creación y sirve de cursor en GET /v1/links.
            $t->id();
            $t->string('link_id', 64)->unique();
            $t->foreignId('instance_id')->constrained('instances');
            $t->string('token_hash', 43)->unique();
            $t->string('state', 16)->index();
            $t->dateTime('expires_at')->index();
            $t->unsignedInteger('max_downloads')->nullable();
            $t->unsignedInteger('downloads')->default(0);
            $t->string('cipher_version', 8);
            $t->unsignedBigInteger('encrypted_size');
            $t->unsignedInteger('part_size');
            $t->longText('meta_blob');
            // SHA-256 del cuerpo de apertura normalizado (idempotencia).
            $t->char('request_hash', 64);
            // SHA-256 hex del paquete ensamblado, fijado al completar.
            $t->char('package_sha256', 64)->nullable();
            $t->string('failure_code', 32)->nullable();
            $t->dateTime('activated_at')->nullable();
            $t->dateTime('exhausted_at')->nullable();
            // Última parte recibida o apertura (subidas abandonadas, C5).
            $t->dateTime('last_activity_at');
            // Cuándo se borraron los bytes; null mientras existen.
            $t->dateTime('purged_at')->nullable();
            $t->timestamps();
        });

        Schema::create('link_parts', function (Blueprint $t) {
            $t->foreignId('link_id')->constrained('links')->cascadeOnDelete();
            $t->unsignedInteger('part_number');
            $t->unsignedInteger('size');
            $t->char('sha256', 64);
            $t->timestamps();
            $t->primary(['link_id', 'part_number']);
        });

        Schema::create('nonces', function (Blueprint $t) {
            $t->foreignId('instance_id')->constrained('instances')->cascadeOnDelete();
            $t->char('nonce', 22);
            $t->dateTime('seen_at')->index();
            // La inserción falla si el nonce ya existe: sin carreras.
            $t->primary(['instance_id', 'nonce']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('nonces');
        Schema::dropIfExists('link_parts');
        Schema::dropIfExists('links');
        Schema::dropIfExists('instances');
    }
};
