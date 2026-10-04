package com.lectable.app.playback

import android.content.Context
import androidx.annotation.OptIn
import androidx.media3.common.PlaybackParameters
import androidx.media3.common.audio.AudioProcessor
import androidx.media3.common.audio.AudioProcessorChain
import androidx.media3.common.audio.SonicAudioProcessor
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.audio.AudioSink
import androidx.media3.exoplayer.audio.DefaultAudioSink
import androidx.media3.exoplayer.audio.SilenceSkippingAudioProcessor

/**
 * Lets [ParagraphPlayer] play faster than ExoPlayer's own ceiling: DefaultAudioSink clamps every
 * requested speed to [DefaultAudioSink.MAX_PLAYBACK_SPEED] (8x) before handing it to its audio
 * processor chain, but takes the speed the chain says it applied as the truth for position
 * tracking. So this chain applies [speed] itself - set it before the player's own
 * playbackParameters, which still has to change for the sink to re-apply - whenever it is past
 * that ceiling, and the clamped value otherwise.
 */
@OptIn(UnstableApi::class)
class FastSpeedRenderersFactory(context: Context) : DefaultRenderersFactory(context) {
    /** The speed actually wanted; only read when above the sink's own maximum. */
    @Volatile
    var speed = 1f

    override fun buildAudioSink(
        context: Context,
        enableFloatOutput: Boolean,
        enableAudioTrackPlaybackParams: Boolean,
    ): AudioSink {
        val sonic = SonicAudioProcessor()
        val chain = DefaultAudioSink.DefaultAudioProcessorChain(arrayOf<AudioProcessor>(), SilenceSkippingAudioProcessor(), sonic)
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
