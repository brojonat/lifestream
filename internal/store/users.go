package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// User is an allowlisted owner. Every event row is scoped to one.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
	CreatedAt time.Time `json:"created_at"`
}

// AddUser allowlists a phone number and returns the user with a fresh API
// token. Only the token's hash is stored, so this is the one chance to see
// it.
func (s *Store) AddUser(ctx context.Context, name, phone string) (User, string, error) {
	if name == "" || phone == "" {
		return User{}, "", fmt.Errorf("adding user: name and phone are required")
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	u := User{ID: NewID(time.Now()), Name: name, Phone: phone, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (id, name, phone, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Name, u.Phone, hashToken(token), fmtTime(u.CreatedAt))
	if err != nil {
		return User{}, "", fmt.Errorf("adding user %s: %w", phone, err)
	}
	return u, token, nil
}

// UserByPhone returns the user with this phone number, or ErrNotFound.
func (s *Store) UserByPhone(ctx context.Context, phone string) (User, error) {
	return s.user(ctx, `WHERE phone = ?`, phone)
}

// UserByToken returns the user owning this API token, or ErrNotFound.
func (s *Store) UserByToken(ctx context.Context, token string) (User, error) {
	return s.user(ctx, `WHERE token_hash = ?`, hashToken(token))
}

// UserByID returns one user, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return s.user(ctx, `WHERE id = ?`, id)
}

// Users returns every user, oldest first.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, phone, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) user(ctx context.Context, where string, arg any) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT id, name, phone, created_at FROM users `+where, arg))
}

func scanUser(row scanner) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Name, &u.Phone, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("reading user: %w", err)
	}
	u.CreatedAt, err = parseTime(created)
	return u, err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
