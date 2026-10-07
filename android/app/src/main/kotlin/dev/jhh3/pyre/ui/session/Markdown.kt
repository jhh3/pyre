package dev.jhh3.pyre.ui.session

import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.Layout
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.unit.Constraints
import androidx.compose.ui.unit.dp

/**
 * A small Markdown subset for agent and user messages: headings, bullet and
 * numbered lists, quotes, rules, fenced code (scrolls sideways), pipe tables
 * (scroll sideways) and inline bold / italic / `code` / ~~strike~~ / links.
 * No dependency; anything it does not recognise stays plain text.
 */
object Md {
    sealed interface Block
    data class Heading(val level: Int, val text: String) : Block
    data class Para(val text: String) : Block
    data class Code(val lang: String, val text: String) : Block
    data class ListBlock(val ordered: Boolean, val items: List<ListItem>) : Block
    data class ListItem(val marker: String, val text: String, val depth: Int)
    data class Quote(val text: String) : Block
    data class Table(val header: List<String>, val rows: List<List<String>>) : Block
    data object Rule : Block

    private val heading = Regex("""^(#{1,6})\s+(.*?)\s*#*\s*$""")
    private val bullet = Regex("""^(\s*)([-*+])\s+(.*)$""")
    private val ordered = Regex("""^(\s*)(\d{1,3})[.)]\s+(.*)$""")
    private val fence = Regex("""^\s*(```|~~~)\s*([\w+-]*).*$""")
    private val rule = Regex("""^\s*([-*_])(\s*\1){2,}\s*$""")
    private val tableSep = Regex("""^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$""")

    fun parse(src: String): List<Block> {
        val lines = src.replace("\r\n", "\n").split('\n')
        val out = ArrayList<Block>()
        val para = StringBuilder()
        fun flushPara() {
            if (para.isNotBlank()) out.add(Para(para.toString().trim()))
            para.setLength(0)
        }
        var i = 0
        while (i < lines.size) {
            val l = lines[i]
            val f = fence.matchEntire(l)
            when {
                f != null -> {
                    flushPara()
                    val close = f.groupValues[1]
                    val body = ArrayList<String>()
                    i++
                    while (i < lines.size && !lines[i].trimStart().startsWith(close)) { body.add(lines[i]); i++ }
                    out.add(Code(f.groupValues[2], body.joinToString("\n").trimEnd()))
                    i++ // closing fence (or end of text: an unclosed fence still renders as code)
                    continue
                }
                l.isBlank() -> flushPara()
                heading.matches(l) -> {
                    flushPara()
                    val m = heading.matchEntire(l)!!
                    out.add(Heading(m.groupValues[1].length, m.groupValues[2]))
                }
                rule.matches(l) -> { flushPara(); out.add(Rule) }
                l.contains('|') && i + 1 < lines.size && tableSep.matches(lines[i + 1]) -> {
                    flushPara()
                    val header = cells(l)
                    val rows = ArrayList<List<String>>()
                    i += 2
                    while (i < lines.size && lines[i].contains('|') && lines[i].isNotBlank()) { rows.add(cells(lines[i])); i++ }
                    out.add(Table(header, rows))
                    continue
                }
                bullet.matches(l) || ordered.matches(l) -> {
                    flushPara()
                    val isOrdered = ordered.matches(l)
                    val items = ArrayList<ListItem>()
                    while (i < lines.size) {
                        val same = (if (isOrdered) ordered else bullet).matchEntire(lines[i])
                        val other = (if (isOrdered) bullet else ordered).matchEntire(lines[i])
                        // A top-level list of the other kind starts a new block; nested ones stay.
                        if (other != null && other.groupValues[1].length < 2) break
                        val b = same ?: other
                        if (b != null) {
                            val depth = (b.groupValues[1].replace("\t", "    ").length / 2).coerceAtMost(4)
                            val marker = if (bullet.matches(lines[i])) "•" else b.groupValues[2] + "."
                            items.add(ListItem(marker, b.groupValues[3], depth))
                        } else if (lines[i].isNotBlank() && lines[i].startsWith("  ") && items.isNotEmpty()) {
                            // continuation of the previous item
                            val last = items.removeAt(items.lastIndex)
                            items.add(last.copy(text = last.text + " " + lines[i].trim()))
                        } else {
                            break
                        }
                        i++
                    }
                    out.add(ListBlock(isOrdered, items))
                    continue
                }
                l.trimStart().startsWith(">") -> {
                    flushPara()
                    val q = ArrayList<String>()
                    while (i < lines.size && lines[i].trimStart().startsWith(">")) { q.add(lines[i].trimStart().removePrefix(">").trim()); i++ }
                    out.add(Quote(q.joinToString(" ")))
                    continue
                }
                else -> { if (para.isNotEmpty()) para.append('\n'); para.append(l.trim()) }
            }
            i++
        }
        flushPara()
        return out
    }

    private fun cells(l: String): List<String> {
        var t = l.trim()
        if (t.startsWith("|")) t = t.substring(1)
        if (t.endsWith("|") && !t.endsWith("\\|")) t = t.dropLast(1)
        return t.split(Regex("""(?<!\\)\|""")).map { it.trim().replace("\\|", "|") }
    }

    /** Inline markup to styled text. Unmatched markers stay as typed. */
    fun inline(s: String, code: SpanStyle, link: SpanStyle): AnnotatedString = buildAnnotatedString {
        var i = 0
        val n = s.length
        while (i < n) {
            val c = s[i]
            fun span(open: String, close: String = open, style: SpanStyle, recurse: Boolean = true): Boolean {
                if (!s.startsWith(open, i)) return false
                val end = s.indexOf(close, i + open.length)
                if (end <= i + open.length) return false
                val inner = s.substring(i + open.length, end)
                if (inner.isBlank() || (open != "`" && inner.first().isWhitespace())) return false
                pushStyle(style)
                if (recurse) append(inline(inner, code, link)) else append(inner)
                pop()
                i = end + close.length
                return true
            }
            when {
                c == '\\' && i + 1 < n && s[i + 1] in "\\`*_~[]()#|>-" -> { append(s[i + 1]); i += 2 }
                c == '`' && span("`", style = code, recurse = false) -> Unit
                c == '*' && span("**", style = SpanStyle(fontWeight = FontWeight.SemiBold)) -> Unit
                c == '_' && span("__", style = SpanStyle(fontWeight = FontWeight.SemiBold)) -> Unit
                c == '~' && span("~~", style = SpanStyle(textDecoration = TextDecoration.LineThrough)) -> Unit
                c == '*' && span("*", style = SpanStyle(fontStyle = FontStyle.Italic)) -> Unit
                c == '_' && (i == 0 || !s[i - 1].isLetterOrDigit()) && span("_", style = SpanStyle(fontStyle = FontStyle.Italic)) -> Unit
                c == '[' -> {
                    val close = s.indexOf("](", i)
                    val end = if (close > i) s.indexOf(')', close) else -1
                    if (close > i + 1 && end > close) {
                        pushStyle(link); append(inline(s.substring(i + 1, close), code, link)); pop()
                        i = end + 1
                    } else { append(c); i++ }
                }
                else -> { append(c); i++ }
            }
        }
    }
}

/** Renders Markdown [text] with the current content colour. */
@Composable
fun Markdown(text: String, modifier: Modifier = Modifier, style: TextStyle = MaterialTheme.typography.bodyLarge) {
    val blocks = remember(text) { Md.parse(text) }
    val codeBg = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.07f)
    val codeSpan = SpanStyle(fontFamily = FontFamily.Monospace, background = codeBg, fontSize = style.fontSize * 0.9f)
    val linkSpan = SpanStyle(color = MaterialTheme.colorScheme.primary, textDecoration = TextDecoration.Underline)
    Column(modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        blocks.forEach { b ->
            when (b) {
                is Md.Heading -> Text(
                    Md.inline(b.text, codeSpan, linkSpan),
                    style = when (b.level) { 1 -> MaterialTheme.typography.titleLarge; 2 -> MaterialTheme.typography.titleMedium; else -> MaterialTheme.typography.titleSmall },
                    fontWeight = FontWeight.SemiBold,
                )
                is Md.Para -> Text(Md.inline(b.text, codeSpan, linkSpan), style = style)
                is Md.Code -> CodeBlock(b.text)
                is Md.ListBlock -> Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                    b.items.forEach { it ->
                        Row(Modifier.padding(start = (it.depth * 14).dp)) {
                            Text(it.marker, style = style, modifier = Modifier.width(if (b.ordered) 26.dp else 16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                            Text(Md.inline(it.text, codeSpan, linkSpan), style = style)
                        }
                    }
                }
                is Md.Quote -> Row(Modifier.height(IntrinsicSize.Min)) {
                    Box(Modifier.width(3.dp).fillMaxHeight().background(MaterialTheme.colorScheme.outlineVariant, RoundedCornerShape(2.dp)))
                    Text(Md.inline(b.text, codeSpan, linkSpan), style = style, color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(start = 10.dp))
                }
                is Md.Table -> MdTable(b, style, codeSpan, linkSpan)
                Md.Rule -> HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
            }
        }
    }
}

/** Monospace block that scrolls sideways instead of wrapping. */
@Composable
fun CodeBlock(text: String, modifier: Modifier = Modifier, maxLines: Int = Int.MAX_VALUE) {
    Surface(
        color = MaterialTheme.colorScheme.onSurface.copy(alpha = 0.06f),
        shape = RoundedCornerShape(10.dp),
        modifier = modifier.fillMaxWidth().testTag("md-code"),
    ) {
        Box(Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 12.dp, vertical = 10.dp)) {
            Text(text, style = MaterialTheme.typography.bodySmall, fontFamily = FontFamily.Monospace, softWrap = false, maxLines = maxLines)
        }
    }
}

/** Pipe table: columns sized to their widest cell (capped at 260dp), scrolling sideways. */
@Composable
private fun MdTable(t: Md.Table, style: TextStyle, code: SpanStyle, link: SpanStyle) {
    val cols = maxOf(t.header.size, t.rows.maxOfOrNull { it.size } ?: 0)
    val all = listOf(t.header) + t.rows
    val line = MaterialTheme.colorScheme.outlineVariant
    val bold = style.copy(fontWeight = FontWeight.SemiBold)
    Box(Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).testTag("md-table")) {
        Layout(
            content = {
                all.forEachIndexed { r, row ->
                    for (c in 0 until cols) {
                        Text(Md.inline(row.getOrElse(c) { "" }, code, link), style = if (r == 0) bold else style, modifier = Modifier.padding(end = 16.dp, top = 4.dp, bottom = 4.dp))
                    }
                }
                HorizontalDivider(color = line)
            },
        ) { ms, _ ->
            val cap = 260.dp.roundToPx()
            val minCol = 48.dp.roundToPx()
            val cells = ms.dropLast(1)
            val widths = IntArray(cols) { c -> all.indices.maxOf { r -> cells[r * cols + c].maxIntrinsicWidth(Constraints.Infinity).coerceIn(minCol, cap) } }
            val placed = cells.mapIndexed { i, m -> m.measure(Constraints(minWidth = widths[i % cols], maxWidth = widths[i % cols])) }
            val heights = all.indices.map { r -> (0 until cols).maxOf { c -> placed[r * cols + c].height } }
            val w = widths.sum()
            val rule = ms.last().measure(Constraints.fixedWidth(w))
            layout(w, heights.sum() + rule.height) {
                var y = 0
                all.indices.forEach { r ->
                    var x = 0
                    for (c in 0 until cols) { placed[r * cols + c].place(x, y); x += widths[c] }
                    y += heights[r]
                    if (r == 0) { rule.place(0, y); y += rule.height }
                }
            }
        }
    }
}
