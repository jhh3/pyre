package dev.jhh3.pyre.ui.session

import androidx.compose.ui.text.SpanStyle
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class MarkdownTest {
    @Test
    fun blocks() {
        val md = """
            # Plan
            Some **bold** and `code`.

            - one
            - two
              continued
            1. first
            2. second

            ```kotlin
            val x = 1
            ```
            | a | b |
            |---|:-:|
            | 1 | 2 |
            > quoted
            ---
        """.trimIndent()
        val b = Md.parse(md)
        assertEquals(listOf("Heading", "Para", "ListBlock", "ListBlock", "Code", "Table", "Quote", "Rule"), b.map { it::class.simpleName })
        assertEquals("two continued", (b[2] as Md.ListBlock).items[1].text)
        assertEquals("val x = 1", (b[4] as Md.Code).text)
        assertEquals(listOf(listOf("1", "2")), (b[5] as Md.Table).rows)
    }

    @Test
    fun inlineStripsMarkers() {
        val s = Md.inline("a **b** *c* `d*e` [f](https://x) ~~g~~ snake_case_name 2*3", SpanStyle(), SpanStyle())
        assertEquals("a b c d*e f g snake_case_name 2*3", s.text)
        assertTrue(s.spanStyles.size >= 5)
    }

    @Test
    fun unclosedFenceStillCode() {
        val b = Md.parse("text\n```\nopen code")
        assertEquals("open code", (b.last() as Md.Code).text)
    }
}
