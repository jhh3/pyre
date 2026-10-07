package dev.jhh3.pyre.ui.viewer

import android.annotation.SuppressLint
import android.net.http.SslCertificate
import android.net.http.SslError
import android.os.Build
import android.webkit.CookieManager
import android.webkit.SslErrorHandler
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import dev.jhh3.pyre.data.Repository
import dev.jhh3.pyre.domain.Tone
import dev.jhh3.pyre.model.MintPreviewRequest
import dev.jhh3.pyre.net.CertPins
import dev.jhh3.pyre.net.HubEndpoint
import dev.jhh3.pyre.net.PyreApiException
import dev.jhh3.pyre.ui.common.Banner
import dev.jhh3.pyre.ui.common.ErrorBox
import dev.jhh3.pyre.ui.common.LoadingBox
import java.io.ByteArrayInputStream
import java.security.cert.CertificateFactory
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull

data class PreviewUi(val url: String? = null, val error: String? = null, val loading: Boolean = true)

/**
 * Mints a fresh one-time preview link (`artifacts.preview_url`, v1.2) right
 * before the sandboxed WebView opens it. The link carries its own short-lived
 * ticket, never the management bearer token, and may live on a separate
 * preview origin.
 */
class HtmlPreviewViewModel(private val repo: Repository, private val endpoint: HubEndpoint, val versionId: String) : ViewModel() {
    private val _ui = MutableStateFlow(PreviewUi())
    val ui: StateFlow<PreviewUi> = _ui.asStateFlow()

    init { mint() }

    fun mint() {
        _ui.value = PreviewUi(loading = true)
        viewModelScope.launch {
            _ui.value = try {
                val p = repo.call { previewUrl(MintPreviewRequest(versionId = versionId)) }
                resolve(endpoint, p.url)?.let { PreviewUi(url = it.toString(), loading = false) }
                    ?: PreviewUi(error = "The hub returned a preview link this app will not open (not an https /preview/ URL).", loading = false)
            } catch (e: PyreApiException) {
                PreviewUi(error = "No isolated preview available: ${e.describe()}. HTML is not rendered without one.", loading = false)
            }
        }
    }

    companion object {
        /**
         * Accepts a hub-relative `/preview/...` link or an absolute https link
         * to a (possibly separate) preview origin. Never a template, never
         * cleartext except the labelled loopback developer mode.
         */
        fun resolve(endpoint: HubEndpoint, previewUrl: String): HttpUrl? {
            if (previewUrl.isEmpty() || previewUrl.contains("{") || previewUrl.contains("}")) return null
            val url = previewUrl.toHttpUrlOrNull() ?: if (previewUrl.startsWith("/")) endpoint.resolve(previewUrl) else return null
            val schemeOk = url.scheme == "https" || (endpoint.devCleartext && url.scheme == "http" && url.host in HubEndpoint.DEV_HOSTS)
            if (!schemeOk) return null
            if (url.username.isNotEmpty() || url.password.isNotEmpty()) return null
            if (!url.encodedPath.contains("/preview/")) return null
            return url
        }
    }
}

@Composable
fun HtmlPreviewScreen(vm: HtmlPreviewViewModel, endpoint: HubEndpoint) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    Column(Modifier.fillMaxSize()) {
        Banner("Safe preview: scripts, cookies, storage and links are turned off for this page.", Tone.MUTED)
        when {
            ui.loading -> LoadingBox()
            ui.error != null -> ErrorBox(ui.error!!, vm::mint)
            ui.url != null -> AndroidView(
                factory = { ctx -> sandboxedWebView(ctx, endpoint, ui.url!!) },
                modifier = Modifier.fillMaxSize().testTag("html-preview"),
            )
            else -> Text("Nothing to show.", Modifier.padding(16.dp))
        }
    }
}

private fun sandboxedWebView(ctx: android.content.Context, endpoint: HubEndpoint, url: String): WebView {
    val allowed = url.toHttpUrlOrNull()!!
    return WebView(ctx).apply {
        settings.apply {
            javaScriptEnabled = false
            javaScriptCanOpenWindowsAutomatically = false
            setSupportMultipleWindows(false)
            allowFileAccess = false
            allowContentAccess = false
            domStorageEnabled = false
            setGeolocationEnabled(false)
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            cacheMode = WebSettings.LOAD_NO_CACHE
            safeBrowsingEnabled = true
        }
        val cookies = CookieManager.getInstance()
        cookies.setAcceptCookie(false)
        cookies.setAcceptThirdPartyCookies(this, false)
        // No addJavascriptInterface: the page gets no bridge into the app.
        webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean = true

            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                val u = request.url.toString().toHttpUrlOrNull()
                val sameOrigin = u != null && u.host == allowed.host && u.port == allowed.port && u.scheme == allowed.scheme
                return if (sameOrigin) null else WebResourceResponse("text/plain", "utf-8", 403, "Blocked", emptyMap(), ByteArrayInputStream(ByteArray(0)))
            }

            @SuppressLint("WebViewClientOnReceivedSslError")
            override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: SslError) {
                // Self-signed hub: proceed ONLY if the leaf certificate matches the paired pin.
                val leaf = x509(error.certificate)
                if (endpoint.pinned && leaf != null && CertPins.matches(leaf, endpoint.certSha256) &&
                    error.url.toHttpUrlOrNull()?.host == allowed.host
                ) handler.proceed() else handler.cancel()
            }
        }
        loadUrl(url)
    }
}

private fun x509(cert: SslCertificate): java.security.cert.X509Certificate? = try {
    if (Build.VERSION.SDK_INT >= 29) {
        cert.x509Certificate
    } else {
        SslCertificate.saveState(cert).getByteArray("x509-certificate")?.let {
            CertificateFactory.getInstance("X.509").generateCertificate(ByteArrayInputStream(it)) as java.security.cert.X509Certificate
        }
    }
} catch (_: Exception) {
    null
}
