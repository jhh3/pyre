// Root build: plugin versions only; applied in :app.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.android.library) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
    // Applied by :app only when app/google-services.json exists (fcm flavor).
    alias(libs.plugins.google.services) apply false
}
