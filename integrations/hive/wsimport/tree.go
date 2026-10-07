package wsimport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message is one ws mailbox message.
type Message struct {
	Mailbox     string            `json:"mailbox"` // directory = authoritative recipient
	File        string            `json:"file"`    // root-relative path of the primary copy
	State       string            `json:"state"`   // "unread" | "read"
	Pending     bool              `json:"pending"` // a reply obligation copy exists in pending/
	ID          string            `json:"legacy_id,omitempty"`
	From        string            `json:"from,omitempty"`
	To          string            `json:"to,omitempty"`
	Kind        string            `json:"kind,omitempty"`
	Sev         string            `json:"sev,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	ExpectReply bool              `json:"expect_reply,omitempty"`
	Created     string            `json:"created,omitempty"`
	CreatedAt   time.Time         `json:"-"`
	Body        string            `json:"-"`
	Extra       map[string]string `json:"-"` // unknown frontmatter keys
	NoFront     bool              `json:"-"` // file had no parseable frontmatter
}

// Key is the stable per-mailbox identity: the ws id, else the file stem.
func (m Message) Key() string {
	if m.ID != "" {
		return m.ID
	}
	return "file:" + strings.TrimSuffix(filepath.Base(m.File), ".md")
}

// Stream is the subset of STREAM.md the migration needs.
type Stream struct {
	Slug             string         `json:"slug"`
	Title            string         `json:"title,omitempty"`
	Status           string         `json:"status,omitempty"`
	Harness          string         `json:"harness,omitempty"`
	HarnessSessionID string         `json:"harness_session_id,omitempty"`
	Channels         []string       `json:"channels,omitempty"`
	Cursors          map[string]int `json:"cursors,omitempty"`
}

// Tree is a loaded ws root copy.
type Tree struct {
	Root      string                   `json:"root"`
	Streams   map[string]Stream        `json:"-"`
	Mailboxes []string                 `json:"-"`
	Messages  []Message                `json:"-"`
	Channels  map[string][]ChannelPost `json:"-"`
	Problems  []string                 `json:"problems,omitempty"`
	// Digest covers every mail file's path and content plus channel
	// subscriptions/cursors: the migration watermark's identity.
	Digest  string    `json:"digest"`
	TakenAt time.Time `json:"taken_at"`
}

// ErrLiveRoot is returned when asked to read the live ws root directly.
var ErrLiveRoot = errors.New("wsimport: refusing to read the live ws root; snapshot it first (--snapshot-from) or pass --allow-live")

// LiveRoots returns the ws root(s) this user's ws would use (WS_ROOT, else
// ~/streams), resolved.
func LiveRoots(env func(string) string) []string {
	var out []string
	if v := env("WS_ROOT"); v != "" {
		out = append(out, v)
	}
	if h := env("HOME"); h != "" {
		out = append(out, filepath.Join(h, "streams"))
	}
	for i, p := range out {
		out[i] = canonical(p)
	}
	return out
}

// canonical is the absolute, symlink-resolved form of p (best effort).
func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func isLive(root string, live []string) bool {
	r := canonical(root)
	for _, l := range live {
		if canonical(l) == r {
			return true
		}
	}
	return false
}

// Load reads a ws root copy. It refuses a live root unless allowLive.
func Load(root string, live []string, allowLive bool, now time.Time) (*Tree, error) {
	if !allowLive && isLive(root, live) {
		return nil, ErrLiveRoot
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("wsimport: %s is not a directory", root)
	}
	t := &Tree{Root: root, Streams: map[string]Stream{}, Channels: map[string][]ChannelPost{}, TakenAt: now}
	h := sha256.New()
	digestFile := func(rel string, data []byte) {
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\x00%x\n", rel, sum)
	}

	// Streams.
	sdirs, _ := filepath.Glob(filepath.Join(root, "streams", "*", "STREAM.md"))
	sort.Strings(sdirs)
	for _, p := range sdirs {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Problems = append(t.Problems, "unreadable "+rel(root, p))
			continue
		}
		meta, _, _ := parseDoc(string(data))
		slug := filepath.Base(filepath.Dir(p))
		s := Stream{Slug: slug, Title: meta["title"], Status: meta["status"], Harness: meta["harness"],
			HarnessSessionID: meta["session_id"], Channels: splitList(meta["channels"]), Cursors: parseCursors(meta["chan_pos"])}
		t.Streams[slug] = s
		fmt.Fprintf(h, "stream:%s\x00channels=%s\x00chan_pos=%s\n", slug, meta["channels"], meta["chan_pos"])
	}

	// Mailboxes.
	mdirs, _ := os.ReadDir(filepath.Join(root, "inbox"))
	for _, d := range mdirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		t.Mailboxes = append(t.Mailboxes, d.Name())
	}
	sort.Strings(t.Mailboxes)
	for _, mb := range t.Mailboxes {
		byKey := map[string]int{}
		add := func(m Message) {
			if i, ok := byKey[m.Key()]; ok {
				// Same message seen twice (moved unread->read during a copy, or a
				// pending copy): the read observation wins; obligations merge.
				if m.State == "read" {
					t.Messages[i].State = "read"
					t.Messages[i].File = m.File
				}
				t.Messages[i].Pending = t.Messages[i].Pending || m.Pending
				return
			}
			byKey[m.Key()] = len(t.Messages)
			t.Messages = append(t.Messages, m)
		}
		for _, sub := range []struct{ dir, state string }{{"", "unread"}, {"read", "read"}, {"pending", "pending"}} {
			files, _ := filepath.Glob(filepath.Join(root, "inbox", mb, sub.dir, "*.md"))
			sort.Strings(files)
			for _, p := range files {
				if strings.HasPrefix(filepath.Base(p), ".") {
					continue
				}
				data, err := os.ReadFile(p)
				if err != nil {
					t.Problems = append(t.Problems, "unreadable "+rel(root, p))
					continue
				}
				digestFile(rel(root, p), data)
				m := parseMessage(mb, rel(root, p), string(data))
				switch sub.state {
				case "pending":
					m.State, m.Pending = "read", true
				default:
					m.State = sub.state
				}
				add(m)
			}
		}
	}

	// Channels.
	cfiles, _ := filepath.Glob(filepath.Join(root, "channels", "*.md"))
	sort.Strings(cfiles)
	for _, p := range cfiles {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Problems = append(t.Problems, "unreadable "+rel(root, p))
			continue
		}
		digestFile(rel(root, p), data)
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		t.Channels[name] = parseChannel(name, string(data))
	}
	t.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return t, nil
}

func parseMessage(mailbox, relPath, text string) Message {
	meta, body, ok := parseDoc(text)
	m := Message{Mailbox: mailbox, File: relPath, NoFront: !ok, Body: strings.TrimSpace(body), Extra: map[string]string{}}
	for k, v := range meta {
		switch k {
		case "id":
			m.ID = v
		case "from":
			m.From = v
		case "to":
			m.To = v
		case "kind":
			m.Kind = v
		case "sev":
			m.Sev = v
		case "reply_to":
			m.ReplyTo = v
		case "expect_reply":
			m.ExpectReply = v == "true"
		case "created":
			m.Created = v
			m.CreatedAt = parseCreated(v)
		default:
			m.Extra[k] = v
		}
	}
	return m
}

func rel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// CopyTree snapshots the mail-relevant parts of a ws root (inbox/**,
// channels/*.md, streams/*/STREAM.md) into dst, which must not exist or be
// empty. The source is only read. Returns the number of files copied.
func CopyTree(src, dst string) (int, error) {
	if ents, err := os.ReadDir(dst); err == nil && len(ents) > 0 {
		return 0, fmt.Errorf("wsimport: snapshot destination %s is not empty", dst)
	}
	n := 0
	copyFile := func(p string) error {
		r := rel(src, p)
		out := filepath.Join(dst, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // moved by ws mid-copy (unread -> read); the other copy is taken
			}
			return err
		}
		defer in.Close()
		fi, err := in.Stat()
		if err != nil {
			return err
		}
		o, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(o, in); err != nil {
			o.Close()
			return err
		}
		if err := o.Close(); err != nil {
			return err
		}
		n++
		return os.Chtimes(out, fi.ModTime(), fi.ModTime())
	}
	err := filepath.WalkDir(filepath.Join(src, "inbox"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Keep empty mailboxes: they exist (every stream has one) and the
			// inventory must count them.
			return os.MkdirAll(filepath.Join(dst, filepath.FromSlash(rel(src, p))), 0o700)
		}
		if name == ".lock" || strings.HasPrefix(name, ".ws-tmp-") {
			return nil
		}
		return copyFile(p)
	})
	if err != nil {
		return n, err
	}
	for _, pattern := range []string{filepath.Join(src, "channels", "*.md"), filepath.Join(src, "streams", "*", "STREAM.md")} {
		files, _ := filepath.Glob(pattern)
		for _, p := range files {
			if err := copyFile(p); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
