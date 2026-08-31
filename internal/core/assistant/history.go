package assistant

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
)

// conversationTTL bounds conversation history and debug traces: working data
// for an active build, not permanent storage. Refreshed on each write so an
// in-use conversation never expires mid-session.
const conversationTTL = 7 * 24 * time.Hour

// historyByteBudget bounds the replayed history by content bytes; oldest
// messages are dropped first. Without it a long-lived conversation grows past
// the model's context and every later turn fails.
const historyByteBudget = 64 << 10

// keyScope prefixes every Redis key with the authenticated caller and project.
// conversation_id is client-minted, so without this any authenticated caller
// who learns another's id reads and appends to their conversation.
func keyScope(creds CallerCredentials, conversationID string) string {
	return creds.ProjectID + ":" + creds.CustomerID + ":" + conversationID
}

func historyKey(creds CallerCredentials, conversationID string) string {
	return "conversation:" + keyScope(creds, conversationID) + ":messages"
}

func turnLockKey(creds CallerCredentials, conversationID string) string {
	return "conversation:" + keyScope(creds, conversationID) + ":turn"
}

// acquireTurn takes the conversation's turn lock. History is a plain GET at
// turn start and SET at turn end, so a second concurrent turn (another tab, a
// client retry while the first stream is still running) would drop one turn's
// exchange. The TTL matches turnTimeout so a crashed process cannot wedge the
// conversation.
func acquireTurn(ctx context.Context, rdb *redis.Client, creds CallerCredentials, conversationID string) (release func(), err error) {
	key := turnLockKey(creds, conversationID)
	token := rand.Text()
	ok, err := rdb.SetNX(ctx, key, token, turnTimeout).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrTurnInProgress
	}
	return func() { _ = releaseTurnScript.Run(context.WithoutCancel(ctx), rdb, []string{key}, token).Err() }, nil
}

// A turn that overran turnTimeout finds its lock expired and possibly re-taken;
// a plain DEL would release the newer turn's lock.
var releaseTurnScript = redis.NewScript(
	`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) end return 0`)

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
	messages = trimHistory(messages)
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

// trimHistory keeps the newest messages that fit historyByteBudget.
func trimHistory(messages []*aidashboardsv1.Message) []*aidashboardsv1.Message {
	total := 0
	for i, m := range slices.Backward(messages) {
		total += len(m.GetContent())
		if total > historyByteBudget {
			return messages[i+1:]
		}
	}
	return messages
}
