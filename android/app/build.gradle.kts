import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

// ---------------------------------------------------------------------------
// Push: the default build is Firebase-free. When app/google-services.json is
// present (never committed), an `fcm` product flavor is added next to `oss`
// and the google-services plugin is applied. Without the file there are no
// flavors at all, so `assembleDebug`, `testDebugUnitTest` and `lintDebug`
// work unchanged.
// ---------------------------------------------------------------------------
val googleServicesFile = file("google-services.json")
val fcmAvailable = googleServicesFile.isFile
if (fcmAvailable) {
    apply(plugin = "com.google.gms.google-services")
}

// ---------------------------------------------------------------------------
// Release signing. Secrets live OUTSIDE the repository in
// ~/.config/pyre/android-signing.properties (override the path with
// -PpyreSigningProperties=/abs/path for throwaway verification builds):
//   storeFile=/abs/path/pyre-release.jks
//   storePassword=...
//   keyAlias=pyre
//   keyPassword=...
// Without that file, release builds are produced unsigned.
// ---------------------------------------------------------------------------
val signingPropsFile: File = (findProperty("pyreSigningProperties") as String?)
    ?.let { File(it) }
    ?: File(System.getProperty("user.home"), ".config/pyre/android-signing.properties")
val signingProps = Properties().apply {
    if (signingPropsFile.isFile) signingPropsFile.inputStream().use { load(it) }
}
val releaseSigningConfigured = listOf("storeFile", "storePassword", "keyAlias", "keyPassword")
    .all { !signingProps.getProperty(it).isNullOrBlank() }

val fixturesDir = rootDir.parentFile.resolve("tests/fixtures/contracts")
val digestFixturesDir = rootDir.parentFile.resolve("tests/fixtures/digest")

android {
    namespace = "dev.jhh3.pyre"
    // Compose BOM 2026.09 libraries require compiling against API 37; behaviour targets 36.
    compileSdk = 37

    defaultConfig {
        applicationId = "dev.jhh3.pyre"
        minSdk = 26
        targetSdk = 36
        versionCode = (findProperty("pyreVersionCode") as String?)?.toInt() ?: 13
        versionName = (findProperty("pyreVersionName") as String?) ?: "0.2.11"
        buildConfigField("int", "PYRE_PROTOCOL", "1")
        buildConfigField("boolean", "FCM_ENABLED", "false")
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    signingConfigs {
        if (releaseSigningConfigured) {
            create("release") {
                storeFile = File(signingProps.getProperty("storeFile").replaceFirst(Regex("^~"), System.getProperty("user.home")))
                storePassword = signingProps.getProperty("storePassword")
                keyAlias = signingProps.getProperty("keyAlias")
                keyPassword = signingProps.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        debug {
            // Separate identity so a debug build never collides with the
            // release-signed install (different signing keys).
            applicationIdSuffix = ".debug"
            resValue("string", "app_name", "Pyre (debug)")
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            resValue("string", "app_name", "Pyre")
            signingConfig = signingConfigs.findByName("release")
        }
    }

    if (fcmAvailable) {
        flavorDimensions += "push"
        productFlavors {
            create("oss") { dimension = "push" }
            create("fcm") {
                dimension = "push"
                buildConfigField("boolean", "FCM_ENABLED", "true")
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
        resValues = true
    }

    testOptions {
        unitTests {
            isIncludeAndroidResources = true
            all { test ->
                test.systemProperty("pyre.fixtures.dir", fixturesDir.absolutePath)
                test.inputs.dir(fixturesDir).withPropertyName("pyreContractFixtures")
                test.systemProperty("pyre.digest.fixtures.dir", digestFixturesDir.absolutePath)
                test.inputs.dir(digestFixturesDir).withPropertyName("pyreDigestFixtures")
                test.maxHeapSize = "2g"
                // Opt-in integration run against pyre-mockhub (see scripts/mockhub-it.sh).
                project.properties.filterKeys { it.startsWith("pyre.mockhub.") || it.startsWith("pyre.realhub.") || it == "pyre.digest.render" }
                    .forEach { (k, v) -> test.systemProperty(k, v.toString()) }
            }
        }
    }

    lint {
        abortOnError = true
        checkDependencies = false
        // Version-bump advice is not actionable inside a pinned build.
        disable += setOf("GradleDependency", "NewerVersionAvailable", "AndroidGradlePluginVersion", "OldTargetApi")
        // aapt auto-versions <adaptive-icon> to -v26; an unversioned mipmap-anydpi
        // leaves no default resource, so the -v26 folder is required.
        disable += "ObsoleteSdkInt"
    }

    packaging {
        resources.excludes += setOf("/META-INF/{AL2.0,LGPL2.1}", "META-INF/versions/9/OSGI-INF/MANIFEST.MF")
    }
}

// Built-in Kotlin (AGP 9): jvmTarget follows compileOptions.targetCompatibility (17).

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.service)
    // Only to read and migrate the pre-S-G EncryptedSharedPreferences store.
    implementation(libs.androidx.security.crypto)
    implementation(libs.androidx.datastore.preferences)
    implementation(project(":termux-terminal"))
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.okhttp)

    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.ui.tooling.preview)
    debugImplementation(libs.androidx.compose.ui.tooling)
    debugImplementation(libs.androidx.compose.ui.test.manifest)

    if (fcmAvailable) {
        "fcmImplementation"(libs.firebase.messaging)
        "fcmImplementation"(libs.androidx.fragment)
    }

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver3)
    testImplementation(libs.okhttp.tls)
    testImplementation(libs.robolectric)
    testImplementation(libs.androidx.test.core)
    testImplementation(libs.androidx.test.ext.junit)
    testImplementation(platform(libs.androidx.compose.bom))
    testImplementation(libs.androidx.compose.ui.test.junit4)
}
