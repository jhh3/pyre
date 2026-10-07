# kotlinx.serialization ships its own R8 rules; keep our @Serializable models'
# generated serializers reachable (they are looked up reflectively via serializer<T>()).
-keepattributes *Annotation*, InnerClasses, Signature
-keepclassmembers @kotlinx.serialization.Serializable class dev.jhh3.pyre.** {
    static ** Companion;
    *** Companion;
    kotlinx.serialization.KSerializer serializer(...);
}
-keep class dev.jhh3.pyre.**$$serializer { *; }

# Optional FCM glue is located reflectively so the default build has no Firebase.
-keep class dev.jhh3.pyre.push.FcmPushProvider { *; }

# OkHttp optional platform integrations.
-dontwarn org.bouncycastle.**
-dontwarn org.conscrypt.**
-dontwarn org.openjsse.**

# Tink (via androidx.security:security-crypto) references compile-only
# errorprone annotations.
-dontwarn com.google.errorprone.annotations.**
