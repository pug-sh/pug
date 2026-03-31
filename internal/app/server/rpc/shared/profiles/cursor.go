package profiles

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// profileListCursor is a keyset pagination cursor for the profiles list.
// It encodes the create_time and id of the last returned row, used as a
// seek point for the next page. Matches the ORDER BY create_time DESC, id DESC.
type profileListCursor struct {
	CreateTime time.Time `json:"t"`
	ID         string    `json:"i"`
}

// encode returns the cursor as a base64url-encoded JSON string for use as a page token.
func (c *profileListCursor) encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode profile list cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeProfileListCursor decodes a base64url-encoded JSON page token.
// Returns an error if the token is malformed or missing required fields.
func decodeProfileListCursor(token string) (*profileListCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	var c profileListCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("invalid page token: %w", err)
	}
	if c.CreateTime.IsZero() || c.ID == "" {
		return nil, fmt.Errorf("invalid page token: missing required cursor fields")
	}
	return &c, nil
}
