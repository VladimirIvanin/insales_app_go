package insales

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Account is a shop account.
type Account struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// GetAccount retrieves GET /admin/account.json.
func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	raw, err := c.do(ctx, http.MethodGet, "/admin/account.json", nil)
	if err != nil {
		return nil, err
	}
	var account Account
	if err := json.Unmarshal(raw, &account); err != nil {
		return nil, &ResourceError{Err: fmt.Errorf("invalid JSON: %w", err)}
	}
	if account.ID == 0 {
		return nil, &ResourceError{Err: fmt.Errorf("missing account.id in response")}
	}
	return &account, nil
}
