package com.lectable.app.ui.jobs

import org.junit.Assert.assertEquals
import org.junit.Test

class UpgradeTiersTest {
    @Test
    fun `only more urgent tiers are offered`() {
        assertEquals(listOf("urgent", "lookahead", "normal"), upgradeTiers("background"))
        assertEquals(listOf("urgent"), upgradeTiers("lookahead"))
        assertEquals(emptyList<String>(), upgradeTiers("urgent"))
        assertEquals(emptyList<String>(), upgradeTiers("some-future-tier"))
    }
}
