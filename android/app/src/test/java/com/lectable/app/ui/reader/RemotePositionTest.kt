package com.lectable.app.ui.reader

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class RemotePositionTest {
    @Test
    fun ignoresOwnPositionAndOwnSaves() {
        assertNull(remotePosition(null, 0 to 0, emptyList(), null))
        assertNull(remotePosition(2 to 5, 2 to 5, emptyList(), null))
        assertNull(remotePosition(2 to 4, 2 to 6, listOf(2 to 4, 2 to 5), null))
    }

    @Test
    fun reportsMovedPositionUntilDismissed() {
        assertEquals(9 to 1, remotePosition(9 to 1, 2 to 6, listOf(2 to 4), null))
        assertNull(remotePosition(9 to 1, 2 to 6, emptyList(), 9 to 1))
        assertEquals(9 to 2, remotePosition(9 to 2, 2 to 6, emptyList(), 9 to 1))
    }
}
