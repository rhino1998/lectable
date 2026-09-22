package com.lectable.app

import com.lectable.app.data.remote.applyLiveOps
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Test

/** Patches in the exact wire shape backend internal/live.Op marshals to. */
class LiveOpsTest {

    private fun apply(doc: String, ops: String) =
        applyLiveOps(Json.parseToJsonElement(doc), Json.parseToJsonElement(ops) as JsonArray)

    private fun assertJson(expected: String, actual: Any) =
        assertEquals(Json.parseToJsonElement(expected), actual)

    @Test
    fun `set replaces a nested field and adds a new key`() {
        val out = apply(
            """{"paragraphs":[{"idx":0,"s":"pending"},{"idx":1,"s":"pending"}]}""",
            """[{"op":"set","path":["paragraphs",0,"s"],"value":"ready"},{"op":"set","path":["paragraphs",0,"url"],"value":"/a"}]""",
        )
        assertJson("""{"paragraphs":[{"idx":0,"s":"ready","url":"/a"},{"idx":1,"s":"pending"}]}""", out)
    }

    @Test
    fun `untouched siblings keep their identity`() {
        val doc = Json.parseToJsonElement("""{"paragraphs":[{"idx":0},{"idx":1}]}""")
        val out = applyLiveOps(doc, Json.parseToJsonElement("""[{"op":"set","path":["paragraphs",0,"x"],"value":1}]""") as JsonArray)
        assertSame(doc.jsonObject["paragraphs"]!!.jsonArray[1], out.jsonObject["paragraphs"]!!.jsonArray[1])
    }

    @Test
    fun `del removes an object key, null and false values survive`() {
        assertJson(
            """{"a":null,"b":false}""",
            apply("""{"a":1,"b":true,"c":3}""", """[{"op":"del","path":["c"]},{"op":"set","path":["a"],"value":null},{"op":"set","path":["b"],"value":false}]"""),
        )
    }

    @Test
    fun `arr rebuilds from index runs and literals`() {
        // A job queue shifting by one: drop the head, keep the rest, append one.
        assertJson(
            """{"q":[{"id":"b"},{"id":"c"},{"id":"d"}]}""",
            apply("""{"q":[{"id":"a"},{"id":"b"},{"id":"c"}]}""", """[{"op":"arr","path":["q"],"items":[[1,3],{"v":{"id":"d"}}]}]"""),
        )
        assertJson("[]", apply("[1,2]", """[{"op":"arr","path":[],"items":[]}]"""))
    }

    @Test
    fun `root set replaces the whole document`() {
        assertJson("""{"a":1}""", apply("[1,2]", """[{"op":"set","path":[],"value":{"a":1}}]"""))
    }
}
