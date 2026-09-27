package com.lectable.app.playback.lighting

import java.io.IOException
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.Base64
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.X509TrustManager
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody

/** The subset of a Hue light's state this app reads and writes - frontend lighting/hue.ts's
 *  HueState. transitionTime is in deciseconds, Hue's own unit. */
data class HueState(
    val on: Boolean? = null,
    val bri: Int? = null,
    val xy: List<Double>? = null,
    val ct: Int? = null,
    val colorMode: String? = null,
    val transitionTime: Int? = null,
) {
    fun toJson(): JsonObject = buildJsonObject {
        on?.let { put("on", it) }
        bri?.let { put("bri", it) }
        xy?.let { v -> putJsonArray("xy") { v.forEach { add(JsonPrimitive(it)) } } }
        ct?.let { put("ct", it) }
        transitionTime?.let { put("transitiontime", it) }
    }
}

data class HueGroup(val id: String, val name: String, val type: String, val lights: List<String>)

data class HueLight(val id: String, val name: String, val state: HueState, val color: Boolean)

data class DiscoveredBridge(val id: String, val ip: String)

class HueException(val type: Int, message: String) : IOException(message)

/** The bridge's error type while pairing waits on the physical link button. */
const val HUE_LINK_BUTTON_NOT_PRESSED = 101

private val JSON_MEDIA = "application/json".toMediaType()

/** A successful pairing: the v1 API username plus the pin of the certificate the bridge presented
 *  while pairing (see [BridgeTrust]). */
data class HuePairing(val username: String, val certPin: String)

/**
 * How a bridge's TLS certificate is trusted. Current bridge firmware 301-redirects every plain-HTTP
 * request to HTTPS, and serves a certificate issued by Philips' own "root-bridge" CA (CN = the
 * bridge id, not its IP) - which no system trust store has, hence "Trust anchor for certification
 * path not found" from a default client. So: with no [pin] yet (pairing, or a pairing saved before
 * pins existed), accept only a certificate whose issuer is Philips Hue's root-bridge, and record
 * its public-key pin; once pinned, accept only that exact key. Hostname verification is off - the
 * certificate names the bridge id, never the IP it's reached at - trust rests on the pin instead.
 */
private class BridgeTrust(private val pin: String?) : X509TrustManager {
    @Volatile var seenPin: String? = null
        private set

    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        val leaf = chain?.firstOrNull() ?: throw CertificateException("bridge sent no certificate")
        val leafPin = certPin(leaf)
        if (pin != null) {
            if (leafPin != pin) throw CertificateException("bridge certificate changed since pairing - forget and re-pair the bridge")
        } else {
            val issuer = leaf.issuerX500Principal.name
            if (!issuer.contains("CN=root-bridge") || !issuer.contains("O=Philips Hue")) {
                throw CertificateException("not a Hue bridge certificate (issuer $issuer)")
            }
        }
        seenPin = leafPin
    }

    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
        throw CertificateException("client certificates aren't used")

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()

    companion object {
        fun certPin(cert: X509Certificate): String =
            Base64.getEncoder().encodeToString(MessageDigest.getInstance("SHA-256").digest(cert.publicKey.encoded))
    }
}

private fun bridgeHttp(trust: BridgeTrust): OkHttpClient {
    val ssl = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), SecureRandom()) }
    return OkHttpClient.Builder()
        .connectTimeout(4, TimeUnit.SECONDS)
        .readTimeout(4, TimeUnit.SECONDS)
        .sslSocketFactory(ssl.socketFactory, trust)
        .hostnameVerifier { _, _ -> true } // see BridgeTrust
        .build()
}

/**
 * A minimal client for a Philips Hue bridge's local v1 REST API (`https://<bridge>/api/...`) -
 * the Kotlin twin of frontend/src/lighting/hue.ts. v1 because it's all this needs (rooms, light
 * and group state), over HTTPS with [BridgeTrust]'s pinning. Owns its own [OkHttpClient]s: the
 * app's shared one rewrites every request's host to the backend (DynamicBaseUrlInterceptor), which
 * would send these to the wrong place. [certPin] is the pin saved at pairing ("" for none yet).
 */
class HueBridgeClient(private val ip: String, private val username: String, certPin: String) {

    private val http = bridgeHttp(BridgeTrust(certPin.ifBlank { null }))

    private fun url(path: String) = "https://$ip/api/$username$path"

    private suspend fun get(url: String) = request(http, Request.Builder().url(url).build())

    private suspend fun put(url: String, body: JsonObject) {
        request(http, Request.Builder().url(url).put(body.toString().toRequestBody(JSON_MEDIA)).build())
    }

    /** Rooms and zones - the units a listener picks to light. */
    suspend fun groups(): List<HueGroup> {
        val raw = get(url("/groups")).jsonObject
        return raw.mapNotNull { (id, v) ->
            val g = v.jsonObject
            val type = g["type"]?.jsonPrimitive?.content ?: return@mapNotNull null
            if (type != "Room" && type != "Zone") return@mapNotNull null
            HueGroup(
                id = id,
                name = g["name"]?.jsonPrimitive?.content ?: id,
                type = type,
                lights = g["lights"]?.jsonArray?.map { it.jsonPrimitive.content }.orEmpty(),
            )
        }
    }

    suspend fun lights(): List<HueLight> {
        val raw = get(url("/lights")).jsonObject
        return raw.map { (id, v) ->
            val l = v.jsonObject
            val s = l["state"]?.jsonObject ?: JsonObject(emptyMap())
            val xy = s["xy"]?.jsonArray?.mapNotNull { it.jsonPrimitive.doubleOrNull }
            val gamut = l["capabilities"]?.jsonObject?.get("control")?.jsonObject?.get("colorgamut")
            HueLight(
                id = id,
                name = l["name"]?.jsonPrimitive?.content ?: id,
                state = HueState(
                    on = s["on"]?.jsonPrimitive?.booleanOrNull,
                    bri = s["bri"]?.jsonPrimitive?.intOrNull,
                    xy = xy,
                    ct = s["ct"]?.jsonPrimitive?.intOrNull,
                    colorMode = s["colormode"]?.jsonPrimitive?.content,
                ),
                color = gamut != null || xy != null,
            )
        }
    }

    /** One light's change - the bridge takes ~10 of these a second across all lights. */
    suspend fun setLight(id: String, state: HueState) {
        put(url("/lights/$id/state"), state.toJson())
    }

    /** Every light in a group at once - only ~1 a second. */
    suspend fun setGroup(id: String, state: HueState) {
        put(url("/groups/$id/action"), state.toJson())
    }

    companion object {
        private val cloud = OkHttpClient.Builder()
            .connectTimeout(4, TimeUnit.SECONDS)
            .readTimeout(4, TimeUnit.SECONDS)
            .build()

        /** Philips' cloud discovery (matches bridges by public IP) - call only on an explicit tap;
         *  manual IP entry is the fallback. */
        suspend fun discover(): List<DiscoveredBridge> =
            request(cloud, Request.Builder().url("https://discovery.meethue.com/").build()).jsonArray.map {
                val o = it.jsonObject
                DiscoveredBridge(
                    id = o["id"]?.jsonPrimitive?.content ?: "",
                    ip = o["internalipaddress"]?.jsonPrimitive?.content ?: "",
                )
            }.filter { it.ip.isNotBlank() }

        /** Registers this app with the bridge, returning the username (v1 API key) and the pin of the
         *  bridge certificate it was obtained over - both to keep. Throws a [HueException] of type
         *  [HUE_LINK_BUTTON_NOT_PRESSED] until the bridge's link button has been pressed - callers
         *  poll. */
        suspend fun pair(ip: String, deviceName: String): HuePairing {
            val trust = BridgeTrust(null)
            val body = buildJsonObject { put("devicetype", "lectable#$deviceName".take(40)) }
            val res = request(
                bridgeHttp(trust),
                Request.Builder().url("https://$ip/api").post(body.toString().toRequestBody(JSON_MEDIA)).build(),
            )
            val username = res.jsonArray.firstOrNull()?.jsonObject?.get("success")?.jsonObject
                ?.get("username")?.jsonPrimitive?.content
                ?: throw HueException(-1, "bridge returned no username")
            return HuePairing(username, trust.seenPin ?: throw HueException(-1, "bridge certificate wasn't seen"))
        }

        private suspend fun request(http: OkHttpClient, req: Request): JsonElement = withContext(Dispatchers.IO) {
            http.newCall(req).execute().use { res ->
                if (!res.isSuccessful) throw HueException(-1, "bridge answered ${res.code}")
                val body = Json.parseToJsonElement(res.body?.string().orEmpty())
                // v1 reports failures as 200 [{"error": {...}}].
                if (body is JsonArray) {
                    body.firstNotNullOfOrNull { (it as? JsonObject)?.get("error")?.jsonObject }?.let { err ->
                        throw HueException(
                            err["type"]?.jsonPrimitive?.intOrNull ?: -1,
                            err["description"]?.jsonPrimitive?.content ?: "bridge error",
                        )
                    }
                }
                body
            }
        }
    }
}
