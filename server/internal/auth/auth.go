// Package auth manages users and login sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CookieName = "rhis_remote_session"
	SessionTTL = 12 * time.Hour
)

var (
	ErrInvalidLogin = errors.New("invalid_login")
	ErrNotFound     = errors.New("not_found")
)

type User struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Language    string     `json:"language"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func (u *User) IsAdmin() bool { return u.Role == "admin" }

type Service struct {
	DB           *pgxpool.Pool
	SecureCookie bool
}

const userCols = `id, email, name, role, language, created_at, last_login_at`

func scanUser(r pgx.Row) (*User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Language, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &u, err
}

func (s *Service) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Service) CreateUser(ctx context.Context, email, name, password, role, lang string) (*User, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	if role != "admin" && role != "technician" {
		return nil, errors.New("invalid_role")
	}
	if lang != "nl" {
		lang = "en"
	}
	h, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	return scanUser(s.DB.QueryRow(ctx,
		`INSERT INTO users(email, name, password_hash, role, language) VALUES(lower($1),$2,$3,$4,$5) RETURNING `+userCols,
		strings.TrimSpace(email), strings.TrimSpace(name), h, role, lang))
}

// CreateFirstAdmin creates the first admin only while there are no users, atomically.
func (s *Service) CreateFirstAdmin(ctx context.Context, email, name, password, lang string) (*User, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	h, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, errors.New("setup_done")
	}
	if lang != "nl" {
		lang = "en"
	}
	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users(email, name, password_hash, role, language) VALUES(lower($1),$2,$3,'admin',$4) RETURNING `+userCols,
		strings.TrimSpace(email), strings.TrimSpace(name), h, lang))
	if err != nil {
		return nil, err
	}
	return u, tx.Commit(ctx)
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Service) UpdateUser(ctx context.Context, id int64, name, role, lang string) error {
	if role != "admin" && role != "technician" {
		return errors.New("invalid_role")
	}
	if lang != "nl" {
		lang = "en"
	}
	tag, err := s.DB.Exec(ctx, `UPDATE users SET name=$2, role=$3, language=$4 WHERE id=$1`, id, name, role, lang)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) SetLanguage(ctx context.Context, id int64, lang string) error {
	if lang != "nl" {
		lang = "en"
	}
	_, err := s.DB.Exec(ctx, `UPDATE users SET language=$2 WHERE id=$1`, id, lang)
	return err
}

func (s *Service) SetPassword(ctx context.Context, id int64, pw string) error {
	if err := ValidatePassword(pw); err != nil {
		return err
	}
	h, err := HashPassword(pw)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, id, h); err != nil {
		return err
	}
	// Changing a password signs the user out everywhere.
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteUser refuses to delete the last admin.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var admins int
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1`, id).Scan(&role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
		return err
	}
	if role == "admin" && admins <= 1 {
		return errors.New("last_admin")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Login checks the password and creates a session. It returns the raw token for the cookie.
func (s *Service) Login(ctx context.Context, email, password, ip, ua string) (*User, string, error) {
	var hash string
	var id int64
	err := s.DB.QueryRow(ctx, `SELECT id, password_hash FROM users WHERE email=lower($1)`, strings.TrimSpace(email)).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Spend the same time as a real check so emails cannot be probed by timing.
		CheckPassword(dummyHash, password)
		return nil, "", ErrInvalidLogin
	}
	if err != nil {
		return nil, "", err
	}
	ok, err := CheckPassword(hash, password)
	if err != nil || !ok {
		return nil, "", ErrInvalidLogin
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	th := sha256.Sum256([]byte(tok))
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash, user_id, expires_at, ip, user_agent) VALUES($1,$2,$3,$4,$5)`,
		th[:], id, time.Now().Add(SessionTTL), ip, ua); err != nil {
		return nil, "", err
	}
	s.DB.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, id)
	s.DB.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	u, err := scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
	return u, tok, err
}

var dummyHash, _ = HashPassword("dummy-password-for-timing")

func (s *Service) Logout(ctx context.Context, tok string) {
	th := sha256.Sum256([]byte(tok))
	s.DB.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, th[:])
}

func (s *Service) UserForToken(ctx context.Context, tok string) (*User, error) {
	th := sha256.Sum256([]byte(tok))
	return scanUser(s.DB.QueryRow(ctx,
		`SELECT u.id, u.email, u.name, u.role, u.language, u.created_at, u.last_login_at
		   FROM sessions s JOIN users u ON u.id=s.user_id
		  WHERE s.token_hash=$1 AND s.expires_at > now()`, th[:]))
}

func (s *Service) SetCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.SecureCookie,
		SameSite: http.SameSiteLaxMode, MaxAge: int(SessionTTL.Seconds())})
}

func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.SecureCookie,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

type ctxKey struct{}

func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// Verify checks a user's password without creating a session.
func (s *Service) Verify(ctx context.Context, id int64, password string) error {
	var hash string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1`, id).Scan(&hash); err != nil {
		return ErrInvalidLogin
	}
	if ok, err := CheckPassword(hash, password); err != nil || !ok {
		return ErrInvalidLogin
	}
	return nil
}
