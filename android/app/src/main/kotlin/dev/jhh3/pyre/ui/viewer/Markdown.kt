package dev.jhh3.pyre.ui.viewer

import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp

/**
 * Deliberately small Markdown renderer for briefs, docs and text artifacts:
 * headings, paragraphs, bullet/numbered lists, block quotes, fenced code,
 * and inline `code` / **bold** / *italic*. No HTML, links are shown as text
 * (no navigation from untrusted content).
 */
sealed interface MdBlock {
    data class Heading(val level: Int, val text: String) : MdBlock
    data class Paragraph(val text: String) : MdBlock
    data class Bullet(val text: String, val ordinal: String?) : MdBlock
    data class Quote(val text: String) : MdBlock
    data class Code(val text: String) : MdBlock
    data class Table(val rows: List<List<String>>) : MdBlock
}

object MarkdownParser {
    fun parse(src: String): List<MdBlock> {
        val out = ArrayList<MdBlock>()
        val lines = src.replace("\r\n", "\n").split('\n')
        var i = 0
        val para = StringBuilder()
        fun flush() {
            if (para.isNotBlank()) out += MdBlock.Paragraph(para.toString().trim())
            para.clear()
        }
        while (i < lines.size) {
            val line = lines[i]
            val t = line.trimStart()
            when {
                t.startsWith("```") -> {
                    flush()
                    val code = StringBuilder()
                    i++
                    while (i < lines.size && !lines[i].trimStart().startsWith("```")) { code.appendLine(lines[i]); i++ }
                    out += MdBlock.Code(code.toString().trimEnd('\n'))
                }
                t.startsWith("|") -> {
                    flush()
                    val rows = ArrayList<List<String>>()
                    while (i < lines.size && lines[i].trimStart().startsWith("|")) {
                        val cells = lines[i].trim().removePrefix("|").removeSuffix("|").split('|').map { it.trim() }
                        if (!cells.all { it.matches(Regex(":?-{2,}:?")) }) rows += cells
                        i++
                    }
                    i--
                    out += MdBlock.Table(rows)
                }
                t.startsWith("#") && t.takeWhile { it == '#' }.length in 1..6 && t.drop(t.takeWhile { it == '#' }.length).startsWith(" ") -> {
                    flush()
                    val level = t.takeWhile { it == '#' }.length
                    out += MdBlock.Heading(level, t.drop(level).trim())
                }
                t.startsWith("- ") || t.startsWith("* ") || t.startsWith("+ ") -> { flush(); out += MdBlock.Bullet(t.drop(2), null) }
                Regex("^\\d+[.)] ").containsMatchIn(t) -> { flush(); out += MdBlock.Bullet(t.substringAfter(' '), t.substringBefore(' ')) }
                t.startsWith(">") -> { flush(); out += MdBlock.Quote(t.drop(1).trim()) }
                t.isBlank() -> flush()
                else -> para.append(t).append(' ')
            }
            i++
        }
        flush()
        return out
    }

    fun inline(s: String, codeBg: androidx.compose.ui.graphics.Color): AnnotatedString = buildAnnotatedString {
        var i = 0
        while (i < s.length) {
            when {
                s.startsWith("`", i) && s.indexOf('`', i + 1) > i -> {
                    val end = s.indexOf('`', i + 1)
                    withStyle(SpanStyle(fontFamily = FontFamily.Monospace, background = codeBg)) { append(s.substring(i + 1, end)) }
                    i = end + 1
                }
                s.startsWith("**", i) && s.indexOf("**", i + 2) > i -> {
                    val end = s.indexOf("**", i + 2)
                    withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append(s.substring(i + 2, end)) }
                    i = end + 2
                }
                (s[i] == '*' || s[i] == '_') && s.indexOf(s[i], i + 1) > i + 1 -> {
                    val end = s.indexOf(s[i], i + 1)
                    withStyle(SpanStyle(fontStyle = FontStyle.Italic)) { append(s.substring(i + 1, end)) }
                    i = end + 1
                }
                else -> { append(s[i]); i++ }
            }
        }
    }
}

@Composable
fun MarkdownView(src: String, modifier: Modifier = Modifier, inset: Boolean = true) {
    val codeBg = MaterialTheme.colorScheme.surfaceContainerHighest
    Column(modifier.padding(horizontal = if (inset) 16.dp else 0.dp)) {
        MarkdownParser.parse(src).forEach { b ->
            when (b) {
                is MdBlock.Heading -> Text(
                    MarkdownParser.inline(b.text, codeBg),
                    style = when (b.level) { 1 -> MaterialTheme.typography.headlineSmall; 2 -> MaterialTheme.typography.titleLarge; else -> MaterialTheme.typography.titleMedium },
                    modifier = Modifier.padding(top = 12.dp, bottom = 4.dp),
                )
                is MdBlock.Paragraph -> Text(MarkdownParser.inline(b.text, codeBg), style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(vertical = 4.dp))
                is MdBlock.Table -> Column(Modifier.fillMaxWidth().padding(vertical = 6.dp)) {
                    b.rows.forEachIndexed { r, cells ->
                        Row(
                            Modifier.fillMaxWidth()
                                .background(if (r == 0) codeBg else androidx.compose.ui.graphics.Color.Transparent, RoundedCornerShape(6.dp))
                                .padding(horizontal = 8.dp, vertical = 6.dp),
                        ) {
                            cells.forEach { c ->
                                Text(
                                    MarkdownParser.inline(c, codeBg),
                                    style = MaterialTheme.typography.bodyMedium,
                                    fontWeight = if (r == 0) FontWeight.SemiBold else null,
                                    modifier = Modifier.weight(1f).padding(end = 8.dp),
                                )
                            }
                        }
                    }
                }
                is MdBlock.Bullet -> Text(
                    AnnotatedString((b.ordinal ?: "•") + "  ") + MarkdownParser.inline(b.text, codeBg),
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(start = 8.dp, top = 2.dp),
                )
                is MdBlock.Quote -> Text(MarkdownParser.inline(b.text, codeBg), style = MaterialTheme.typography.bodyMedium.copy(fontStyle = FontStyle.Italic), modifier = Modifier.padding(start = 12.dp, top = 4.dp))
                is MdBlock.Code -> Text(
                    b.text,
                    fontFamily = FontFamily.Monospace,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.fillMaxWidth().padding(vertical = 6.dp)
                        .background(codeBg, RoundedCornerShape(6.dp))
                        .horizontalScroll(rememberScrollState())
                        .padding(8.dp),
                )
            }
        }
    }
}
