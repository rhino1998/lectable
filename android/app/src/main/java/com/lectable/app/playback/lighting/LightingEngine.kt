package com.lectable.app.playback.lighting

import com.lectable.app.data.remote.dto.LightEffects
import com.lectable.app.data.remote.dto.LightingDto
import kotlin.math.PI
import kotlin.math.max
import kotlin.math.roundToInt
import kotlin.math.sin
import kotlin.random.Random
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

private const val TICK_MS = 125L
private const val GROUP_MIN_INTERVAL_MS = 1000L
private const val CUT_TRANSITION_DS = 25
private const val CONTINUATION_TRANSITION_DS = 100
private const val PULSE_PERIOD_MS = 4500.0
private const val SHIMMER_PERIOD_MS = 11000.0
private const val LIGHTNING_MIN_GAP_MS = 7000L
private const val LIGHTNING_MAX_GAP_MS = 22000L
private const val MAX_IN_FLIGHT = 2
// A gentle candle-like wander, not a strobe - see frontend engine.ts's FLICKER_DEPTH.
private const val FLICKER_DEPTH = 0.4
private const val FLICKER_COLOR_SHIFT = 0.4
private const val FLICKER_EASE = 0.8
private const val FLICKER_MIN_DS = 3
// A siren or alarm beacon on a 2s cycle - see frontend engine.ts's FLASH_PERIOD_MS.
private const val FLASH_PERIOD_MS = 2000L
private const val FLASH_DIM = 0.15
private const val FLASH_TRANSITION_DS = 2
private const val FLASH_MAX_LIGHTS = 8
private val DEFAULT_BASE = Rgb(1.0, 0.85, 0.7)
private val DEFAULT_FLASH = Rgb(0.86, 0.9, 1.0)

/**
 * Turns the ambience pass's lighting cues into Hue commands for one room, animating each cue's
 * effect locally - a port of frontend/src/lighting/engine.ts (see its header for the rate-limit
 * reasoning); keep the two in step. A new cue goes out as one group command faded over the
 * region's transition; flicker/pulse/shimmer then animate one light per tick, round-robin;
 * lightning is an occasional flash-and-back pair of group commands; flash (a siren or alarm)
 * flips each light once per half cycle. Effects run only while narration plays.
 *
 * Driven from [HueLightingController]'s main-thread scope; every field is touched only there.
 */
class LightingEngine(
    private val scope: CoroutineScope,
    private val bridge: HueBridgeClient,
    private val groupId: String,
    private val lights: List<HueLight>,
    private var cap: Float,
    private val onError: (String) -> Unit,
    private val onOk: () -> Unit,
) {
    private var cue: LightingDto? = null
    private var cutTransition = true
    private var playing = false
    private var baseDirty = false
    private var lastGroupSend = 0L
    private var effectsFrom = 0L
    private var nextLight = 0
    private var nextFlash = 0L
    private var inFlight = 0
    private var loop: Job? = null
    private val snapshot = lights.map { it.id to restorable(it.state) }
    // Each light's flicker position, 0 (dimmest) to 1 - see FLICKER_DEPTH.
    private val flame = DoubleArray(lights.size) { 1.0 }
    // The half cycle each light (and, for a big room, the group) last flipped to.
    private val flashBeat = LongArray(lights.size) { -1 }
    private var groupBeat = -1L

    fun start() {
        loop = scope.launch {
            while (isActive) {
                tick()
                delay(TICK_MS)
            }
        }
    }

    /** Stops the loop and, if [restore], puts every light back the way [start] found it. */
    suspend fun stop(restore: Boolean) {
        loop?.cancel()
        loop = null
        if (!restore) return
        withContext(NonCancellable) {
            for ((id, state) in snapshot) {
                runCatching { bridge.setLight(id, state.copy(transitionTime = 10)) }
                delay(TICK_MS)
            }
        }
    }

    fun setCue(next: LightingDto?, cut: Boolean) {
        if (next == null || sameCue(next, cue)) return
        cue = next
        cutTransition = cut
        baseDirty = true
        scheduleFlash(now())
    }

    fun setCap(value: Float) {
        if (value == cap) return
        cap = value
        baseDirty = true
    }

    fun setPlaying(value: Boolean) {
        if (value == playing) return
        playing = value
        if (!value) baseDirty = true
    }

    private fun now() = System.currentTimeMillis()

    private fun tick() {
        val cue = cue ?: return
        val now = now()
        if (baseDirty) {
            if (now - lastGroupSend < GROUP_MIN_INTERVAL_MS) return
            val ds = if (cutTransition) CUT_TRANSITION_DS else CONTINUATION_TRANSITION_DS
            baseDirty = false
            effectsFrom = now + ds * 100L
            lastGroupSend = now
            send { bridge.setGroup(groupId, baseState(cue, ds)) }
            return
        }
        if (!playing || now < effectsFrom) return
        when (cue.effect) {
            LightEffects.LIGHTNING ->
                if (now >= nextFlash && now - lastGroupSend >= GROUP_MIN_INTERVAL_MS) flash(cue, now)
            LightEffects.FLICKER, LightEffects.PULSE, LightEffects.SHIMMER -> animateNextLight(cue, now)
            LightEffects.FLASH -> flashNext(cue, now)
            else -> {}
        }
    }

    private fun baseState(cue: LightingDto, transitionDs: Int) = HueState(
        on = true,
        bri = hueBrightness(cue.brightness.toDouble(), cap),
        xy = rgbToXy(parseHex(cue.color) ?: DEFAULT_BASE),
        transitionTime = transitionDs,
    )

    private fun animateNextLight(cue: LightingDto, now: Long) {
        if (lights.isEmpty() || inFlight >= MAX_IN_FLIGHT) return
        val i = nextLight % lights.size
        nextLight = i + 1
        val light = lights[i]
        val intervalMs = TICK_MS * lights.size
        val base = parseHex(cue.color) ?: DEFAULT_BASE
        val accent = parseHex(cue.accent)
        var rgb = base
        var level = 1.0
        var ds = max(1, (intervalMs / 100.0).roundToInt())
        when (cue.effect) {
            LightEffects.FLICKER -> {
                val f = flame[i] + (Random.nextDouble() - flame[i]) * FLICKER_EASE
                flame[i] = f
                level = 1 - FLICKER_DEPTH * (1 - f)
                rgb = mixRgb(base, flickerTint(base), (1 - f) * FLICKER_COLOR_SHIFT)
                ds = max(FLICKER_MIN_DS, ds)
            }
            LightEffects.PULSE -> {
                val phase = now / PULSE_PERIOD_MS * 2 * PI
                level = 0.55 + 0.45 * (0.5 + 0.5 * sin(phase))
            }
            LightEffects.SHIMMER -> {
                val phase = now / SHIMMER_PERIOD_MS * 2 * PI + i * 1.7
                if (accent != null) rgb = mixRgb(base, accent, 0.5 + 0.5 * sin(phase))
                level = 0.85 + 0.15 * sin(phase * 1.3)
            }
        }
        val state = HueState(
            on = true,
            bri = hueBrightness(cue.brightness * level, cap),
            xy = if (light.color) rgbToXy(rgb) else null,
            transitionTime = ds,
        )
        send { bridge.setLight(light.id, state) }
    }

    /** Flips the next light that hasn't yet taken the current half cycle's state, or the whole
     *  group in a room too big to flip per light. */
    private fun flashNext(cue: LightingDto, now: Long) {
        val beat = now / (FLASH_PERIOD_MS / 2)
        val n = lights.size
        if (n > FLASH_MAX_LIGHTS) {
            if (beat == groupBeat || now - lastGroupSend < GROUP_MIN_INTERVAL_MS) return
            groupBeat = beat
            lastGroupSend = now
            val state = flashState(cue, beat, color = true)
            send { bridge.setGroup(groupId, state) }
            return
        }
        if (inFlight >= MAX_IN_FLIGHT) return
        for (k in 0 until n) {
            val i = (nextLight + k) % n
            if (flashBeat[i] == beat) continue
            flashBeat[i] = beat
            nextLight = i + 1
            val light = lights[i]
            // With an accent, neighbours sit on opposite colors.
            val lightBeat = if (!cue.accent.isNullOrEmpty() && light.color) beat + i else beat
            val state = flashState(cue, lightBeat, light.color)
            send { bridge.setLight(light.id, state) }
            return
        }
    }

    /** One side of the flash: the cue itself on even beats, and on odd ones its accent - or, with
     *  no accent (or a bulb with no color), the cue dimmed. */
    private fun flashState(cue: LightingDto, beat: Long, color: Boolean): HueState {
        val accent = if (color) parseHex(cue.accent) else null
        val odd = beat % 2 != 0L
        val rgb = if (odd && accent != null) accent else parseHex(cue.color) ?: DEFAULT_BASE
        return HueState(
            on = true,
            bri = hueBrightness(cue.brightness * (if (odd && accent == null) FLASH_DIM else 1.0), cap),
            xy = if (color) rgbToXy(rgb) else null,
            transitionTime = FLASH_TRANSITION_DS,
        )
    }

    private fun flash(cue: LightingDto, now: Long) {
        val strike = HueState(
            on = true,
            bri = hueBrightness(100.0, cap),
            xy = rgbToXy(parseHex(cue.accent) ?: DEFAULT_FLASH),
            transitionTime = 0,
        )
        val back = baseState(cue, 4)
        val double = Random.nextDouble() < 0.35
        lastGroupSend = now
        scheduleFlash(now)
        send {
            bridge.setGroup(groupId, strike)
            delay(140)
            bridge.setGroup(groupId, back)
            if (double) {
                delay(320)
                bridge.setGroup(groupId, strike)
                delay(110)
                bridge.setGroup(groupId, back)
            }
        }
    }

    private fun scheduleFlash(now: Long) {
        nextFlash = now + Random.nextLong(LIGHTNING_MIN_GAP_MS, LIGHTNING_MAX_GAP_MS)
    }

    private fun send(block: suspend () -> Unit) {
        inFlight++
        scope.launch {
            try {
                block()
                onOk()
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                onError(e.message ?: e.toString())
            } finally {
                inFlight--
            }
        }
    }
}

/** [base] nudged a little redder - where a flame's color dips as it gutters (frontend engine.ts's
 *  flickerTint). Only its chromaticity reaches the lights; brightness comes from bri. */
private fun flickerTint(base: Rgb) = Rgb(base.r, base.g * 0.85, base.b * 0.7)

/** Only what a later PUT can put back: on/bri plus the color mode the light was actually in. */
private fun restorable(s: HueState): HueState =
    if (s.colorMode == "ct" && s.ct != null) HueState(on = s.on, bri = s.bri, ct = s.ct)
    else HueState(on = s.on, bri = s.bri, xy = s.xy)

private fun sameCue(a: LightingDto, b: LightingDto?): Boolean =
    b != null && a.color == b.color && (a.accent ?: "") == (b.accent ?: "") &&
        a.brightness == b.brightness && a.effect == b.effect
