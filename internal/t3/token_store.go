package t3

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BrowserTokenStore interface {
	Save(ctx context.Context, devcontainerID, token string, expiresAt time.Time) error
	Get(ctx context.Context, devcontainerID string) (browserToken, bool, error)
	Delete(ctx context.Context, devcontainerID string) error
}

type PostgresPairingTokenStore struct {
	pool *pgxpool.Pool
}

func NewPostgresPairingTokenStore(ctx context.Context, url string) (*PostgresPairingTokenStore, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	store := &PostgresPairingTokenStore{pool: pool}
	if err := store.init(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresPairingTokenStore) Close() {
	s.pool.Close()
}

func (s *PostgresPairingTokenStore) init(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS pairing_tokens (
	devcontainer_id  TEXT PRIMARY KEY,
	token            TEXT NOT NULL,
	expires_at       TIMESTAMPTZ NOT NULL,
	issued_at        TIMESTAMPTZ NOT NULL DEFAULT now()
)`)
	return err
}

func (s *PostgresPairingTokenStore) Save(ctx context.Context, devcontainerID, token string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO pairing_tokens (devcontainer_id, token, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT (devcontainer_id) DO UPDATE
SET token = EXCLUDED.token,
    expires_at = EXCLUDED.expires_at,
    issued_at = now()`,
		devcontainerID, token, expiresAt)
	return err
}

func (s *PostgresPairingTokenStore) Get(ctx context.Context, devcontainerID string) (browserToken, bool, error) {
	var token browserToken
	err := s.pool.QueryRow(ctx, `
SELECT token, devcontainer_id, expires_at
FROM pairing_tokens
WHERE devcontainer_id = $1`, devcontainerID).Scan(&token.Token, &token.EnvID, &token.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return browserToken{}, false, nil
	}
	if err != nil {
		return browserToken{}, false, err
	}
	return token, true, nil
}

func (s *PostgresPairingTokenStore) Delete(ctx context.Context, devcontainerID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM pairing_tokens WHERE devcontainer_id = $1`, devcontainerID)
	return err
}
