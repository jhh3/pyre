// Package wsimport migrates Hive's vendored `ws` mail (mailboxes, reply
// obligations, channels) into Pyre's single messaging authority.
//
// It works on a COPY of the ws root and never writes to it. The on-disk
// format it reads is the one implemented by jhh3/hive a8d95a7
// plugins/hive/vendor/ws/bin/ws:
//
//	streams/<slug>/STREAM.md        frontmatter incl. channels, chan_pos
//	inbox/<mailbox>/*.md            unread messages
//	inbox/<mailbox>/read/*.md       messages ws already returned (marked read)
//	inbox/<mailbox>/pending/*.md    copies of read messages that still owe a reply
//	channels/<name>.md              append-only "## <iso> <sender> [<id>]" log
//
// A message file is "---\n<key: value>...\n---\n\n<body>\n" with keys id,
// from, to, kind, sev, reply_to, expect_reply, created (values JSON-quoted
// when needed). Channel cursors (chan_pos) are Python str offsets, i.e.
// Unicode code points, not bytes.
package wsimport

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// parseDoc mirrors ws parse_doc: frontmatter only when the text starts with
// "---\n"; the block ends at the first "\n---"; one leading newline of the
// body is dropped.
func parseDoc(text string) (map[string]string, string, bool) {
	if !strings.HasPrefix(text, "---\n") {
		return map[string]string{}, text, false
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return map[string]string{}, text, false
	}
	end += 4
	block, body := text[4:end], text[end+4:]
	body = strings.TrimPrefix(body, "\n")
	meta := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || !strings.Contains(line, ":") {
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		meta[strings.TrimSpace(k)] = fmUnquote(v)
	}
	return meta, body, true
}

func fmUnquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
	}
	return v
}

// parseCreated parses ws's now_iso() (local time with offset, seconds).
func parseCreated(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

var chanHeaderRE = regexp.MustCompile(`^## (\S+) (\S+) \[([^\]]+)\]$`)

// ChannelPost is one entry of a channel log.
type ChannelPost struct {
	Channel   string    `json:"channel"`
	Index     int       `json:"index"`
	ID        string    `json:"legacy_id"`
	Sender    string    `json:"from"`
	Created   string    `json:"created"`
	CreatedAt time.Time `json:"-"`
	Body      string    `json:"-"`
	// Offset is the code-point offset of the "## " header line, comparable
	// with ws chan_pos cursors.
	Offset int `json:"offset"`
}

// parseChannel splits a channel log the way ws writes it:
// "\n## <iso> <sender> [<id>]\n<text>\n" appended per post.
func parseChannel(name, data string) []ChannelPost {
	var posts []ChannelPost
	var cur *ChannelPost
	var body []string
	offset := 0 // code points consumed so far
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			posts = append(posts, *cur)
		}
		cur, body = nil, nil
	}
	lines := strings.SplitAfter(data, "\n")
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\n")
		if m := chanHeaderRE.FindStringSubmatch(line); m != nil {
			flush()
			cur = &ChannelPost{Channel: name, Index: len(posts), Created: m[1], CreatedAt: parseCreated(m[1]),
				Sender: m[2], ID: m[3], Offset: offset}
		} else if cur != nil {
			body = append(body, line)
		}
		offset += utf8.RuneCountInString(raw)
	}
	flush()
	return posts
}

// parseCursors reads "name=off, name2=off" (ws get_cursors).
func parseCursors(v string) map[string]int {
	out := map[string]int{}
	for _, part := range strings.Split(v, ",") {
		k, off, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(off)); err == nil {
			out[strings.TrimSpace(k)] = n
		}
	}
	return out
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
