package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/RequiDev/encore/internal/domain"
	"github.com/RequiDev/encore/internal/postgres"
	"github.com/RequiDev/encore/internal/store"
)

// Settings is the repository for app_settings, the instance-wide configuration
// an administrator can change at runtime.
//
// Values are stored as jsonb rather than as text so that a setting can grow from
// a boolean into an object without a migration, and so that the database rejects
// a malformed value at write time.
type Settings struct{ db *store.Store }

// NewSettings builds the repository.
func NewSettings(db *store.Store) *Settings { return &Settings{db: db} }

// Get returns the raw JSON stored under a key, or domain.ErrNotFound when the
// key has never been set.
func (r *Settings) Get(ctx context.Context, q store.Querier, key string) (json.RawMessage, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: setting key is required", domain.ErrValidation)
	}
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, key).Scan(&raw); err != nil {
		return nil, postgres.Classify("get setting", err)
	}
	return json.RawMessage(raw), nil
}

// Set stores a value under a key, replacing whatever was there.
func (r *Settings) Set(ctx context.Context, q store.Querier, key string, value any) error {
	if key == "" {
		return fmt.Errorf("%w: setting key is required", domain.ErrValidation)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		// The value itself is never echoed: a setting may hold a credential.
		return fmt.Errorf("%w: setting %q is not JSON-encodable", domain.ErrValidation, key)
	}

	const sql = `
        INSERT INTO app_settings (key, value, updated_at)
        VALUES ($1, $2::jsonb, now())
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`
	if _, err := q.Exec(ctx, sql, key, raw); err != nil {
		return postgres.Classify("set setting", err)
	}
	return nil
}

// GetBool reads a boolean setting, falling back to def when the key is absent.
//
// A key that exists but holds something other than a boolean is an error rather
// than another fallback: silently treating a corrupt value as the default is how
// an instance ends up open to registration without anyone having asked for it.
func (r *Settings) GetBool(ctx context.Context, q store.Querier, key string, def bool) (bool, error) {
	raw, err := r.Get(ctx, q, key)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return def, nil
		}
		return false, err
	}
	return decodeBool(key, raw)
}

// SetBool stores a boolean setting.
func (r *Settings) SetBool(ctx context.Context, q store.Querier, key string, value bool) error {
	return r.Set(ctx, q, key, value)
}

// RegistrationsEnabled reports whether an unknown Spotify identity may create an
// account.
//
// A missing setting reads as closed. The migration seeds the row, so absence
// means something has gone wrong with the settings table, and failing open there
// would let anyone with the instance URL register.
func (r *Settings) RegistrationsEnabled(ctx context.Context, q store.Querier) (bool, error) {
	return r.GetBool(ctx, q, domain.SettingRegistrationsEnabled, false)
}

// SetRegistrationsEnabled opens or closes the instance to new accounts.
func (r *Settings) SetRegistrationsEnabled(ctx context.Context, q store.Querier, enabled bool) error {
	return r.SetBool(ctx, q, domain.SettingRegistrationsEnabled, enabled)
}

// decodeBool reads a JSON boolean, naming the offending key without quoting its
// value.
func decodeBool(key string, raw json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("%w: setting %q is not a boolean", domain.ErrValidation, key)
	}
	return b, nil
}
