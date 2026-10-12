package com.lectable.app.playback

import android.content.Context
import android.os.Handler
import androidx.annotation.OptIn
import androidx.media3.common.PlaybackParameters
import androidx.media3.common.audio.AudioProcessor
import androidx.media3.common.audio.AudioProcessorChain
import androidx.media3.common.audio.SonicAudioProcessor
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.Renderer
import androidx.media3.exoplayer.audio.AudioRendererEventListener
import androidx.media3.exoplayer.audio.AudioSink
import androidx.media3.exoplayer.audio.DefaultAudioSink
import androidx.media3.exoplayer.audio.SilenceSkippingAudioProcessor
import androidx.media3.exoplayer.mediacodec.MediaCodecSelector

/**
 * Lets [ParagraphPlayer] play faster than ExoPlayer's own ceiling: DefaultAudioSink clamps every
 * requested speed to [DefaultAudioSink.MAX_PLAYBACK_SPEED] (8x) before handing it to its audio
 * processor chain, but takes the speed the chain says it applied as the truth for position
 * tracking. So this chain applies [speed] itself - set it before the player's own
 * playbackParameters, which still has to change for the sink to re-apply - whenever it is past
 * that ceiling, and the clamped value otherwise.
 *
 * Also where speaker positions join the audio path: [spatial] is the first processor of the
 * chain (before the speed change, so a clip boundary in its input is exact), and the audio
 * renderer is a [SeatedAudioRenderer], which tells it each clip's seat.
 */
@OptIn(UnstableApi::class)
class FastSpeedRenderersFactory(context: Context) : DefaultRenderersFactory(context) {
    /** The speed actually wanted; only read when above the sink's own maximum. */
    @Volatile
    var speed = 1f

    internal val spatial = SpatialAudioProcessor()

    override fun buildAudioRenderers(
        context: Context,
        extensionRendererMode: Int,
        mediaCodecSelector: MediaCodecSelector,
        enableDecoderFallback: Boolean,
        audioSink: AudioSink,
        eventHandler: Handler,
        eventListener: AudioRendererEventListener,
        out: ArrayList<Renderer>,
    ) {
        // Not super's: that adds a plain MediaCodecAudioRenderer, then extension renderers
        // this app doesn't ship.
        out += SeatedAudioRenderer(
            context, codecAdapterFactory, mediaCodecSelector, enableDecoderFallback, eventHandler, eventListener, audioSink, spatial,
        )
    }

    override fun buildAudioSink(
        context: Context,
        enableFloatOutput: Boolean,
        enableAudioTrackPlaybackParams: Boolean,
    ): AudioSink {
        val sonic = SonicAudioProcessor()
        val chain = DefaultAudioSink.DefaultAudioProcessorChain(arrayOf<AudioProcessor>(spatial), SilenceSkippingAudioProcessor(), sonic)
        return DefaultAudioSink.Builder(context)
            .setEnableFloatOutput(enableFloatOutput)
            .setEnableAudioTrackPlaybackParams(enableAudioTrackPlaybackParams)
            .setAudioProcessorChain(
                object : AudioProcessorChain by chain {
                    override fun applyPlaybackParameters(playbackParameters: PlaybackParameters): PlaybackParameters {
                        val wanted = speed
                        val applied = if (wanted > DefaultAudioSink.MAX_PLAYBACK_SPEED) wanted else playbackParameters.speed
                        sonic.setSpeed(applied)
                        sonic.setPitch(playbackParameters.pitch)
                        return PlaybackParameters(applied, playbackParameters.pitch)
                    }
                },
            )
            .build()
    }
}
