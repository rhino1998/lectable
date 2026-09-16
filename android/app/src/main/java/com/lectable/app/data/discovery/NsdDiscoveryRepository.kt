package com.lectable.app.data.discovery

import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.net.wifi.WifiManager
import android.util.Log
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update

private const val TAG = "NsdDiscoveryRepository"

/**
 * A backend found on the LAN via mDNS, resolved to a connectable address -
 * see [com.lectable.app.data.settings.ServerSettingsRepository] for where a
 * picked one ends up persisted.
 */
data class DiscoveredServer(
    val name: String,
    val host: String,
    val port: Int,
) {
    val url: String get() = "http://$host:$port/"
}

/**
 * Browses for the backend's `_lectable._tcp` mDNS advertisement (see
 * `backend/internal/mdnsadvert`) via Android's [NsdManager], as an
 * alternative to typing in the backend's LAN IP by hand in Settings.
 * Discovery only runs while [startDiscovery] is active - callers should
 * stop it once the Settings screen isn't visible, since it's a standing
 * multicast listener.
 */
@Singleton
class NsdDiscoveryRepository @Inject constructor(
    private val nsdManager: NsdManager,
    private val wifiManager: WifiManager,
) {
    private val _servers = MutableStateFlow<List<DiscoveredServer>>(emptyList())
    val servers: StateFlow<List<DiscoveredServer>> = _servers

    private var discoveryListener: NsdManager.DiscoveryListener? = null
    private var multicastLock: WifiManager.MulticastLock? = null

    // NsdManager only allows one resolveService() call in flight at a time
    // (a second concurrent call throws) - services found while a resolve is
    // already running queue up here instead.
    private val resolveQueue = ArrayDeque<NsdServiceInfo>()
    private var resolving = false

    fun startDiscovery() {
        if (discoveryListener != null) return
        _servers.value = emptyList()

        // NsdManager is supposed to handle multicast itself, but some OEM Wi-Fi power-save
        // filtering drops mDNS packets for apps that don't hold their own lock - harmless
        // to hold one regardless.
        runCatching {
            wifiManager.createMulticastLock("lectable-mdns").apply {
                setReferenceCounted(false)
                acquire()
            }
        }.onSuccess { multicastLock = it }
            .onFailure { Log.w(TAG, "could not acquire multicast lock", it) }

        val listener = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) {}

            override fun onServiceFound(service: NsdServiceInfo) {
                if (!matchesServiceType(service.serviceType)) return
                enqueueResolve(service)
            }

            override fun onServiceLost(service: NsdServiceInfo) {
                _servers.update { list -> list.filterNot { it.name == service.serviceName } }
            }

            override fun onDiscoveryStopped(serviceType: String) {
                discoveryListener = null
            }

            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                Log.w(TAG, "start discovery failed: $errorCode")
                discoveryListener = null
            }

            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {
                Log.w(TAG, "stop discovery failed: $errorCode")
                discoveryListener = null
            }
        }
        discoveryListener = listener
        runCatching { nsdManager.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, listener) }
            .onFailure {
                Log.w(TAG, "discoverServices threw", it)
                discoveryListener = null
            }
    }

    fun stopDiscovery() {
        multicastLock?.let { lock -> runCatching { if (lock.isHeld) lock.release() } }
        multicastLock = null

        val listener = discoveryListener ?: return
        discoveryListener = null
        runCatching { nsdManager.stopServiceDiscovery(listener) }
        resolveQueue.clear()
        resolving = false
    }

    private fun matchesServiceType(serviceType: String) =
        serviceType.trimEnd('.') == SERVICE_TYPE.trimEnd('.')

    @Synchronized
    private fun enqueueResolve(service: NsdServiceInfo) {
        resolveQueue.addLast(service)
        if (!resolving) processNextResolve()
    }

    @Synchronized
    private fun processNextResolve() {
        val next = resolveQueue.removeFirstOrNull()
        if (next == null) {
            resolving = false
            return
        }
        resolving = true
        nsdManager.resolveService(
            next,
            object : NsdManager.ResolveListener {
                override fun onResolveFailed(serviceInfo: NsdServiceInfo, errorCode: Int) {
                    Log.w(TAG, "resolve failed for ${serviceInfo.serviceName}: $errorCode")
                    processNextResolve()
                }

                override fun onServiceResolved(serviceInfo: NsdServiceInfo) {
                    val host = serviceInfo.host?.hostAddress
                    if (host != null) {
                        val found = DiscoveredServer(
                            name = serviceInfo.serviceName,
                            host = host,
                            port = serviceInfo.port,
                        )
                        _servers.update { list -> list.filterNot { it.name == found.name } + found }
                    }
                    processNextResolve()
                }
            },
        )
    }

    companion object {
        // Trailing dot matches how NsdServiceInfo reports resolved service types.
        const val SERVICE_TYPE = "_lectable._tcp."
    }
}
