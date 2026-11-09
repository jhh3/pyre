package site

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// NotesFile is the notes database inside the state directory.
const NotesFile = "notes.json"

// MaxNoteText is the longest note, in characters.
const MaxNoteText = 4000

// Note is one note on a page item.
type Note struct {
	ID      string     `json:"id"`
	Page    string     `json:"page"`
	Item    string     `json:"item"` // a data-note ID, or PageItem for the page as a whole
	Author  string     `json:"author"`
	Name    string     `json:"name,omitempty"`
	Text    string     `json:"text"`
	Created time.Time  `json:"created"`
	Edited  *time.Time `json:"edited,omitempty"`
	// Delivery is set when the page names a session: the Pyre message
	// that carried the note to it, or why it could not be sent.
	Delivery *Delivery `json:"delivery,omitempty"`
}

// Delivery records sending a note to the page's session.
type Delivery struct {
	Session   string    `json:"session"`
	MessageID string    `json:"message_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// NotesDB is the notes file.
type NotesDB struct {
	Notes []Note `json:"notes"`
}

// Store keeps notes in memory and rewrites <dir>/notes.json (0600,
// atomically) on every change. Empty dir keeps them in memory only.
type Store struct {
	mu   sync.Mutex
	path string
	db   NotesDB
}

// OpenStore loads the notes in dir (none yet is fine).
func OpenStore(dir string) (*Store, error) {
	s := &Store{}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s.path = filepath.Join(dir, NotesFile)
	db, err := ReadNotes(dir)
	if err != nil {
		return nil, err
	}
	s.db = db
	return s, nil
}

// ReadNotes reads the notes file in dir (for `pyre site notes`).
func ReadNotes(dir string) (NotesDB, error) {
	var db NotesDB
	b, err := os.ReadFile(filepath.Join(dir, NotesFile))
	if errors.Is(err, os.ErrNotExist) {
		return db, nil
	}
	if err != nil {
		return db, err
	}
	if err := json.Unmarshal(b, &db); err != nil {
		return db, fmt.Errorf("%s: %v", filepath.Join(dir, NotesFile), err)
	}
	return db, nil
}

// List returns notes, oldest first; page "" means every page.
func List(db NotesDB, page string, since time.Time) []Note {
	out := []Note{}
	for _, n := range db.Notes {
		if (page == "" || n.Page == page) && !n.Created.Before(since) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// List returns notes on page ("" for all), oldest first.
func (s *Store) List(page string) []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	return List(s.db, page, time.Time{})
}

// Add stores a new note.
func (s *Store) Add(n Note) (Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n.ID = newID()
	s.db.Notes = append(s.db.Notes, n)
	if err := s.save(); err != nil {
		s.db.Notes = s.db.Notes[:len(s.db.Notes)-1]
		return Note{}, err
	}
	return n, nil
}

// errNotFound and errNotAuthor are mapped to HTTP statuses by the server.
var (
	errNotFound  = errors.New("That note no longer exists.")
	errNotAuthor = errors.New("You can only change your own notes.")
)

// Update applies fn to the note with id; author must match unless "".
func (s *Store) Update(id, author string, fn func(*Note)) (Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Notes {
		n := &s.db.Notes[i]
		if n.ID != id {
			continue
		}
		if author != "" && n.Author != author {
			return Note{}, errNotAuthor
		}
		old := *n
		fn(n)
		if err := s.save(); err != nil {
			*n = old
			return Note{}, err
		}
		return *n, nil
	}
	return Note{}, errNotFound
}

// Delete removes the author's note.
func (s *Store) Delete(id, author string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.db.Notes {
		if n.ID != id {
			continue
		}
		if n.Author != author {
			return errNotAuthor
		}
		old := s.db.Notes
		s.db.Notes = append(append([]Note{}, old[:i]...), old[i+1:]...)
		if err := s.save(); err != nil {
			s.db.Notes = old
			return err
		}
		return nil
	}
	return errNotFound
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.db, "", " ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".notes-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
