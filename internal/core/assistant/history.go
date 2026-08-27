package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
)

// conversationTTL bounds conversation history and debug traces: working data
// for an active build, not permanent storage. Refreshed on each write so an
// in-use conversation never expires mid-session.
const conversationTTL = 7 * 24 * time.Hour

// keyScope prefixes every Redis key with the authenticated caller and project.
// conversation_id is client-minted, so without this any authenticated caller
// who learns another's id reads and appends to their conversation.
func keyScope(creds CallerCredentials, conversationID string) string {
	return creds.ProjectID + ":" + creds.CustomerID + ":" + conversationID
}

func historyKey(creds CallerCredentials, conversationID string) string {
	return "conversation:" + keyScope(creds, conversationID) + ":messages"
}

// storedMessage is the persisted JSON shape: the proto enum number for role
// ({"role":1,"content":"hi"}).
type storedMessage struct {
	Role    int32  `json:"role"`
	Content string `json:"content"`
}

// loadHistory returns the persisted conversation, empty for an unseen id.
// Errors (Redis down, malformed JSON) must be treated as fatal by the caller —
// silently serving empty history reads as "the model forgot everything".
func loadHistory(ctx context.Context, rdb *redis.Client, creds CallerCredentials, conversationID string) ([]*aidashboardsv1.Message, error) {
	raw, err := rdb.Get(ctx, historyKey(creds, conversationID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var parsed []storedMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	messages := make([]*aidashboardsv1.Message, 0, len(parsed))
	for _, m := range parsed {
		messages = append(messages, &aidashboardsv1.Message{
			Role:    aidashboardsv1.Message_Role(m.Role).Enum(),
			Content: proto.String(m.Content),
		})
	}
	return messages, nil
}

func saveHistory(ctx context.Context, rdb *redis.Client, creds CallerCredentials, conversationID string, messages []*aidashboardsv1.Message) error {
	plain := make([]storedMessage, 0, len(messages))
	for _, m := range messages {
		plain = append(plain, storedMessage{Role: int32(m.GetRole()), Content: m.GetContent()})
	}
	payload, err := json.Marshal(plain)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, historyKey(creds, conversationID), payload, conversationTTL).Err()
}
