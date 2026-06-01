package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const stateKeyPrefix = "oauth:state:"
const stateTTL = 10 * time.Minute

type storedState struct {
	Provider     string `json:"provider"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

// StateStore persists OAuth PKCE state in Redis.
type StateStore struct {
	redis *redis.Client
}

func NewStateStore(redis *redis.Client) *StateStore {
	return &StateStore{redis: redis}
}

func NewStateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *StateStore) Save(ctx context.Context, provider ProviderName, redirectURI, codeVerifier, state string) error {
	payload, err := json.Marshal(storedState{
		Provider:     string(provider),
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
	})
	if err != nil {
		return fmt.Errorf("oauth: marshal state: %w", err)
	}
	key := stateKeyPrefix + state
	if err := s.redis.Set(ctx, key, payload, stateTTL).Err(); err != nil {
		return fmt.Errorf("oauth: save state: %w", err)
	}
	return nil
}

// Consume atomically reads and deletes OAuth state (Redis GETDEL).
func (s *StateStore) Consume(ctx context.Context, state string) (*storedState, error) {
	key := stateKeyPrefix + state
	raw, err := s.redis.GetDel(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrInvalidState
	}
	if err != nil {
		return nil, fmt.Errorf("oauth: consume state: %w", err)
	}
	var st storedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("oauth: unmarshal state: %w", err)
	}
	return &st, nil
}
