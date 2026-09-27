package com.lectable.app.playback.lighting

import kotlin.math.pow
import kotlin.math.roundToInt

/** Color math for driving Hue lights from the ambience pass's lighting cues - a port of
 *  frontend/src/lighting/color.ts; keep the two in step. Channels are 0..1. */
data class Rgb(val r: Double, val g: Double, val b: Double)

private val HEX = Regex("^#?([0-9a-fA-F]{6})$")

/** "#rrggbb" (or "rrggbb") to [Rgb]; anything else is null. */
fun parseHex(hex: String?): Rgb? {
    val m = hex?.trim()?.let(HEX::matchEntire) ?: return null
    val n = m.groupValues[1].toInt(16)
    return Rgb(((n shr 16) and 0xff) / 255.0, ((n shr 8) and 0xff) / 255.0, (n and 0xff) / 255.0)
}

fun mixRgb(a: Rgb, b: Rgb, t: Double): Rgb =
    Rgb(a.r + (b.r - a.r) * t, a.g + (b.g - a.g) * t, a.b + (b.b - a.b) * t)

/** sRGB to CIE xy per Philips' developer docs (gamma, then the Wide RGB D65 matrix); the bridge
 *  clamps to each light's own gamut. Black maps to the D65 white point. */
fun rgbToXy(c: Rgb): List<Double> {
    fun lin(v: Double) = if (v > 0.04045) ((v + 0.055) / 1.055).pow(2.4) else v / 12.92
    val r = lin(c.r)
    val g = lin(c.g)
    val b = lin(c.b)
    val x = r * 0.664511 + g * 0.154324 + b * 0.162028
    val y = r * 0.283881 + g * 0.668433 + b * 0.047685
    val z = r * 0.000088 + g * 0.07231 + b * 0.986039
    val sum = x + y + z
    if (sum == 0.0) return listOf(0.3127, 0.329)
    return listOf(round4(x / sum), round4(y / sum))
}

private fun round4(v: Double) = (v * 10000).roundToInt() / 10000.0

/** A cue's 1-100 brightness, scaled by the listener's cap (0..1), onto Hue's 1-254 "bri". */
fun hueBrightness(brightness: Double, cap: Float): Int {
    val frac = (brightness / 100.0 * cap).coerceIn(0.0, 1.0)
    return (1 + frac * 253).roundToInt().coerceIn(1, 254)
}
