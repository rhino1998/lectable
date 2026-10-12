package com.lectable.app.playback

import android.content.Context
import android.os.Handler
import androidx.annotation.OptIn
import androidx.media3.common.C
import androidx.media3.common.Format
import androidx.media3.common.Timeline
import androidx.media3.common.audio.AudioProcessor.AudioFormat
import androidx.media3.common.audio.BaseAudioProcessor
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.audio.AudioRendererEventListener
import androidx.media3.exoplayer.audio.AudioSink
import androidx.media3.exoplayer.audio.MediaCodecAudioRenderer
import androidx.media3.exoplayer.mediacodec.MediaCodecAdapter
import androidx.media3.exoplayer.mediacodec.MediaCodecSelector
import androidx.media3.exoplayer.source.MediaSource
import java.nio.ByteBuffer

/** A clip's seat ([paragraphSeat]), carried as its MediaItem's tag for [SeatedAudioRenderer]. */
internal data class ClipSeat(val seat: Float?)

/**
 * Places narration in the stereo field ([SpatialMixer]): mono in, stereo out. Always active
 * for mono 16-bit audio, even with positions off (when it just copies the voice to both
 * channels, as the system would) - a processor can't be switched in or out without
 * reconfiguring the sink, and the setting should take effect while playing. [mode] and
 * [width] come from the settings, [seat] from [SeatedAudioRenderer] as each clip's audio
 * arrives.
 */
@OptIn(UnstableApi::class)
internal class SpatialAudioProcessor : BaseAudioProcessor() {
    @Volatile
    var mode = SpatialMode.OFF

    @Volatile
    var width = DEFAULT_SPATIAL_WIDTH

    @Volatile
    var seat: Float? = null

    private var mixer: SpatialMixer? = null

    override fun onConfigure(inputAudioFormat: AudioFormat): AudioFormat {
        if (inputAudioFormat.encoding != C.ENCODING_PCM_16BIT || inputAudioFormat.channelCount != 1) return AudioFormat.NOT_SET
        return AudioFormat(inputAudioFormat.sampleRate, 2, C.ENCODING_PCM_16BIT)
    }

    override fun onFlush() {
        mixer = SpatialMixer(inputAudioFormat.sampleRate)
    }

    override fun onReset() {
        mixer = null
    }

    override fun queueInput(inputBuffer: ByteBuffer) {
        val mixer = mixer ?: return
        val output = replaceOutputBuffer(inputBuffer.remaining() * 2)
        mixer.mix(inputBuffer, output, mode, seat?.let { it * SPATIAL_MAX_PAN * width })
        output.flip()
    }
}

/**
 * The audio renderer, telling [processor] whose clip each decoded buffer belongs to. Several
 * clips are queued and decoded ahead of the one being heard, so "the current media item" is
 * the wrong question: the seat has to change at the exact buffer where one clip's audio ends
 * and the next begins. Each stream the renderer is handed is noted with where it starts in
 * renderer time, and every output buffer is matched to its stream by its own timestamp just
 * before it goes to the sink (whose processors run on this same thread).
 */
@OptIn(UnstableApi::class)
internal class SeatedAudioRenderer(
    context: Context,
    codecAdapterFactory: MediaCodecAdapter.Factory,
    mediaCodecSelector: MediaCodecSelector,
    enableDecoderFallback: Boolean,
    eventHandler: Handler?,
    eventListener: AudioRendererEventListener?,
    audioSink: AudioSink,
    private val processor: SpatialAudioProcessor,
) : MediaCodecAudioRenderer(context, codecAdapterFactory, mediaCodecSelector, enableDecoderFallback, eventHandler, eventListener, audioSink) {
    // (start in renderer time, seat) of the stream playing and those read ahead, oldest first.
    private val streams = ArrayDeque<Pair<Long, Float?>>()

    override fun onStreamChanged(formats: Array<Format>, startPositionUs: Long, offsetUs: Long, mediaPeriodId: MediaSource.MediaPeriodId) {
        super.onStreamChanged(formats, startPositionUs, offsetUs, mediaPeriodId)
        // A seek or a changed queue starts over from an earlier time: what was read ahead of
        // it is gone.
        while (streams.isNotEmpty() && streams.last().first >= startPositionUs) streams.removeLast()
        streams.addLast(startPositionUs to seatOf(mediaPeriodId))
    }

    override fun processOutputBuffer(
        positionUs: Long,
        elapsedRealtimeUs: Long,
        codec: MediaCodecAdapter?,
        buffer: ByteBuffer?,
        bufferIndex: Int,
        bufferFlags: Int,
        sampleCount: Int,
        bufferPresentationTimeUs: Long,
        isDecodeOnlyBuffer: Boolean,
        isLastBuffer: Boolean,
        format: Format,
    ): Boolean {
        while (streams.size > 1 && streams[1].first <= bufferPresentationTimeUs) streams.removeFirst()
        streams.firstOrNull()?.let { processor.seat = it.second }
        return super.processOutputBuffer(
            positionUs, elapsedRealtimeUs, codec, buffer, bufferIndex, bufferFlags, sampleCount,
            bufferPresentationTimeUs, isDecodeOnlyBuffer, isLastBuffer, format,
        )
    }

    override fun onDisabled() {
        streams.clear()
        super.onDisabled()
    }

    private fun seatOf(mediaPeriodId: MediaSource.MediaPeriodId): Float? {
        val timeline = timeline
        val periodIdx = timeline.getIndexOfPeriod(mediaPeriodId.periodUid)
        if (periodIdx == C.INDEX_UNSET) return null
        val windowIdx = timeline.getPeriod(periodIdx, Timeline.Period()).windowIndex
        val item = timeline.getWindow(windowIdx, Timeline.Window()).mediaItem
        return (item.localConfiguration?.tag as? ClipSeat)?.seat
    }
}
