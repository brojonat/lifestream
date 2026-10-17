// Package state is the app's mutable state, kept in one KV bucket so a
// single watch wakes a page for any change: the demo items, a preference,
// and a filesystem stamp the watcher bumps when local files change.
package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	fsKey          = "fs"
	itemPrefix     = "item."
	starredOnlyKey = "pref.starred_only"
)

// Item is the demo record. Replace it with whatever the app is about.
type Item struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Note    string    `json:"note"`
	Starred bool      `json:"starred"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Store wraps the bucket.
type Store struct {
	kv jetstream.KeyValue
}

func New(kv jetstream.KeyValue) *Store { return &Store{kv: kv} }

// NewID returns a random 16-hex-char identifier.
func NewID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Item returns one item by id.
func (s *Store) Item(ctx context.Context, id string) (Item, bool, error) {
	e, err := s.kv.Get(ctx, itemPrefix+id)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("reading item: %w", err)
	}
	var it Item
	if err := json.Unmarshal(e.Value(), &it); err != nil {
		return Item{}, false, fmt.Errorf("decoding item: %w", err)
	}
	return it, true, nil
}

// Items returns every item, newest first.
func (s *Store) Items(ctx context.Context) ([]Item, error) {
	var out []Item
	lister, err := s.kv.ListKeys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing items: %w", err)
	}
	defer lister.Stop()
	for key := range lister.Keys() {
		if !strings.HasPrefix(key, itemPrefix) {
			continue
		}
		it, ok, err := s.Item(ctx, strings.TrimPrefix(key, itemPrefix))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Put stores an item, assigning an id and timestamps as needed.
func (s *Store) Put(ctx context.Context, it Item) (Item, error) {
	now := time.Now()
	if it.ID == "" {
		it.ID = NewID()
	}
	if it.Created.IsZero() {
		it.Created = now
	}
	it.Updated = now
	b, err := json.Marshal(it)
	if err != nil {
		return it, fmt.Errorf("encoding item: %w", err)
	}
	if _, err := s.kv.Put(ctx, itemPrefix+it.ID, b); err != nil {
		return it, fmt.Errorf("writing item: %w", err)
	}
	return it, nil
}

// Delete removes an item; a missing id is not an error.
func (s *Store) Delete(ctx context.Context, id string) error {
	err := s.kv.Delete(ctx, itemPrefix+id)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("deleting item: %w", err)
	}
	return nil
}

// StarredOnly reports the "show only starred" preference.
func (s *Store) StarredOnly(ctx context.Context) (bool, error) {
	e, err := s.kv.Get(ctx, starredOnlyKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading preference: %w", err)
	}
	return string(e.Value()) == "1", nil
}

// SetStarredOnly stores the preference, waking every watcher.
func (s *Store) SetStarredOnly(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	if _, err := s.kv.Put(ctx, starredOnlyKey, []byte(v)); err != nil {
		return fmt.Errorf("writing preference: %w", err)
	}
	return nil
}

// Touch records a new filesystem fingerprint, waking every watcher.
func (s *Store) Touch(ctx context.Context, fingerprint string) error {
	if _, err := s.kv.Put(ctx, fsKey, []byte(fingerprint)); err != nil {
		return fmt.Errorf("writing fs stamp: %w", err)
	}
	return nil
}

// Fingerprint returns the last recorded filesystem fingerprint, or "".
func (s *Store) Fingerprint(ctx context.Context) (string, error) {
	e, err := s.kv.Get(ctx, fsKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading fs stamp: %w", err)
	}
	return string(e.Value()), nil
}

// Watch delivers every change to the bucket from now on. Callers render
// current state first, then block on Updates(); a wake means "re-read".
func (s *Store) Watch(ctx context.Context) (jetstream.KeyWatcher, error) {
	w, err := s.kv.WatchAll(ctx, jetstream.UpdatesOnly())
	if err != nil {
		return nil, fmt.Errorf("watching state: %w", err)
	}
	return w, nil
}
