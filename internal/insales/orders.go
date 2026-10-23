package insales

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Order is a compact InSales order.
type Order struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Status string `json:"status,omitempty"`
}

// ListOrders returns GET /admin/orders.json.
func (c *Client) ListOrders(ctx context.Context, page, perPage int) ([]Order, error) {
	raw, err := c.do(ctx, http.MethodGet, withPage("/admin/orders.json", page, perPage), nil)
	if err != nil {
		return nil, err
	}
	var orders []Order
	if err := json.Unmarshal(raw, &orders); err != nil {
		return nil, &ResourceError{Err: fmt.Errorf("expected list of orders: %w", err)}
	}
	return orders, nil
}

// GetOrder returns GET /admin/orders/{id}.json.
func (c *Client) GetOrder(ctx context.Context, id int) (*Order, error) {
	raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/admin/orders/%d.json", id), nil)
	if err != nil {
		return nil, err
	}
	return decodeOrder(raw)
}

// CountOrders returns GET /admin/orders/count.json.
func (c *Client) CountOrders(ctx context.Context) (int, error) {
	return c.count(ctx, "/admin/orders/count.json")
}

// UpdateOrder sends PUT /admin/orders/{id}.json with {"order": patch}.
func (c *Client) UpdateOrder(ctx context.Context, id int, patch map[string]any) (*Order, error) {
	payload, err := json.Marshal(map[string]any{"order": patch})
	if err != nil {
		return nil, &ResourceError{Err: err}
	}
	raw, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/admin/orders/%d.json", id), payload)
	if err != nil {
		return nil, err
	}
	return decodeOrder(raw)
}

func decodeOrder(raw []byte) (*Order, error) {
	var order Order
	if err := json.Unmarshal(raw, &order); err != nil {
		return nil, &ResourceError{Err: fmt.Errorf("expected order object: %w", err)}
	}
	if order.ID == 0 {
		return nil, &ResourceError{Err: fmt.Errorf("missing order.id")}
	}
	return &order, nil
}

func (c *Client) count(ctx context.Context, path string) (int, error) {
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, &ResourceError{Err: fmt.Errorf("expected count object: %w", err)}
	}
	if payload.Count == nil {
		return 0, &ResourceError{Err: fmt.Errorf("missing count")}
	}
	return *payload.Count, nil
}

func withPage(path string, page, perPage int) string {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if perPage > 0 {
		q.Set("per_page", strconv.Itoa(perPage))
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
