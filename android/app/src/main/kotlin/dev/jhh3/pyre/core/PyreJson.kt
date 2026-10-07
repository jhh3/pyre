package dev.jhh3.pyre.core

import kotlinx.serialization.ExperimentalSerializationApi
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonNamingStrategy

/**
 * The single JSON configuration for the Pyre wire protocol.
 *
 * - snake_case names (the Go `json:"..."` tags) via the naming strategy;
 * - unknown keys are ignored so a newer hub can add fields;
 * - `null` for a non-null property with a default decodes as the default
 *   (Go encodes nil slices/maps as null);
 * - encoding omits default values and nulls, reproducing `omitempty`.
 */
@OptIn(ExperimentalSerializationApi::class)
object PyreJson {
    val wire: Json = Json {
        namingStrategy = JsonNamingStrategy.SnakeCase
        ignoreUnknownKeys = true
        coerceInputValues = true
        explicitNulls = false
        encodeDefaults = false
        isLenient = false
    }

    /** Strict variant used by contract tests: any key the models lack fails. */
    val strict: Json = Json(wire) { ignoreUnknownKeys = false }

    /** Local persistence (cache/outbox). Same names, defaults always written. */
    val local: Json = Json(wire) { encodeDefaults = true }
}
