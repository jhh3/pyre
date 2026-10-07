package dev.jhh3.pyre.files

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import androidx.core.content.FileProvider
import dev.jhh3.pyre.model.ArtifactVersion
import dev.jhh3.pyre.model.Paths
import dev.jhh3.pyre.net.HubClient
import java.io.File
import java.security.MessageDigest
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** Downloads immutable artifact bytes. Separate from PyreApi so tests can fake it. */
interface ArtifactContent {
    suspend fun download(version: ArtifactVersion, dest: File)
}

class HubArtifactContent(private val client: HubClient) : ArtifactContent {
    override suspend fun download(version: ArtifactVersion, dest: File) {
        val path = version.contentUrl.ifEmpty { Paths.content(version.versionId) }
        client.get(path).use { resp ->
            withContext(Dispatchers.IO) {
                dest.parentFile?.mkdirs()
                val tmp = File(dest.parentFile, dest.name + ".part")
                resp.body.byteStream().use { input -> tmp.outputStream().use { input.copyTo(it) } }
                if (!tmp.renameTo(dest)) throw java.io.IOException("could not store download")
            }
        }
    }
}

sealed interface Verified {
    data class Ok(val file: File) : Verified
    /** The hub's digest is not a sha256 hex value (e.g. synthetic fixtures); bytes are not verifiable. */
    data class Unverifiable(val file: File, val reason: String) : Verified
    data class Mismatch(val expected: String, val actual: String) : Verified
}

object ArtifactFiles {
    /** Safe local name: never trust the display filename as a path. */
    fun safeName(v: ArtifactVersion): String {
        val base = v.filename.substringAfterLast('/').substringAfterLast('\\')
            .replace(Regex("[^A-Za-z0-9._-]"), "_").trim('.', '_')
        return base.ifEmpty { v.versionId }.take(80)
    }

    fun target(cacheDir: File, v: ArtifactVersion): File =
        File(File(File(cacheDir, "artifacts"), v.versionId.replace(Regex("[^A-Za-z0-9_-]"), "_")), safeName(v))

    fun sha256(f: File): String {
        val md = MessageDigest.getInstance("SHA-256")
        f.inputStream().use { s ->
            val buf = ByteArray(64 * 1024)
            while (true) { val n = s.read(buf); if (n <= 0) break; md.update(buf, 0, n) }
        }
        return md.digest().joinToString("") { "%02x".format(it) }
    }

    fun verify(f: File, v: ArtifactVersion): Verified {
        val expected = v.digest.removePrefix("sha256:").lowercase()
        if (expected.length != 64 || !expected.all { it in '0'..'9' || it in 'a'..'f' }) {
            return Verified.Unverifiable(f, "hub digest '${v.digest}' is not a SHA-256 value")
        }
        if (v.size > 0 && f.length() != v.size) return Verified.Mismatch("${v.size} bytes", "${f.length()} bytes")
        val actual = sha256(f)
        return if (actual == expected) Verified.Ok(f) else Verified.Mismatch(expected, actual)
    }

    /** Downloads (or reuses a verified cached copy) and checks the digest. */
    suspend fun fetch(cacheDir: File, v: ArtifactVersion, content: ArtifactContent): Verified {
        val f = target(cacheDir, v)
        if (f.isFile) {
            val r = withContext(Dispatchers.IO) { verify(f, v) }
            if (r !is Verified.Mismatch) return r
            f.delete()
        }
        content.download(v, f)
        val r = withContext(Dispatchers.IO) { verify(f, v) }
        if (r is Verified.Mismatch) f.delete()
        return r
    }

    fun isText(mediaType: String) = mediaType.startsWith("text/") && !isHtml(mediaType) ||
        mediaType in setOf("application/json", "application/x-yaml", "application/yaml", "application/toml")

    fun isMarkdown(v: ArtifactVersion) = v.mediaType == "text/markdown" || v.filename.endsWith(".md", true)

    fun isHtml(mediaType: String) = mediaType.substringBefore(';').trim() in setOf("text/html", "application/xhtml+xml")

    /** Opens with the platform viewer via a read-only content:// grant. */
    fun openExternally(ctx: Context, file: File, mediaType: String): String? {
        val uri = FileProvider.getUriForFile(ctx, ctx.packageName + ".files", file)
        val view = Intent(Intent.ACTION_VIEW)
            .setDataAndType(uri, mediaType.substringBefore(';').ifEmpty { "application/octet-stream" })
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
        return try {
            ctx.startActivity(Intent.createChooser(view, file.name).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            null
        } catch (_: ActivityNotFoundException) {
            "No app on this device can open $mediaType"
        }
    }
}
