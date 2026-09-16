package com.lectable.app

import com.lectable.app.data.settings.ServerSettingsRepository
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ServerSettingsRepositoryTest {

    @Test
    fun `normalize adds scheme and trailing slash`() {
        assertEquals("http://192.168.1.42:8080/", ServerSettingsRepository.normalize("192.168.1.42:8080"))
        assertEquals("http://192.168.1.42:8080/", ServerSettingsRepository.normalize("http://192.168.1.42:8080"))
        assertEquals("https://example.com/", ServerSettingsRepository.normalize("https://example.com"))
    }

    @Test
    fun `isValid accepts host-and-port, rejects blank`() {
        assertTrue(ServerSettingsRepository.isValid("192.168.1.42:8080"))
        assertTrue(ServerSettingsRepository.isValid("http://localhost:8080"))
        assertFalse(ServerSettingsRepository.isValid(""))
        assertFalse(ServerSettingsRepository.isValid("   "))
    }
}
