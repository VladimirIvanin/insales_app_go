package insales

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Product is a compact InSales product.
type Product struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// ListProducts returns GET /admin/products.json.
func (c *Client) ListProducts(ctx context.Context, page, perPage int) ([]Product, error) {
	raw, err := c.do(ctx, http.MethodGet, withPage("/admin/products.json", page, perPage), nil)
	if err != nil {
		return nil, err
	}
	var products []Product
	if err := json.Unmarshal(raw, &products); err != nil {
		return nil, &ResourceError{Err: fmt.Errorf("expected list of products: %w", err)}
	}
	return products, nil
}

// GetProduct returns GET /admin/products/{id}.json.
func (c *Client) GetProduct(ctx context.Context, id int) (*Product, error) {
	raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/admin/products/%d.json", id), nil)
	if err != nil {
		return nil, err
	}
	return decodeProduct(raw)
}

// CountProducts returns GET /admin/products/count.json.
func (c *Client) CountProducts(ctx context.Context) (int, error) {
	return c.count(ctx, "/admin/products/count.json")
}

// UpdateProduct sends PUT /admin/products/{id}.json with {"product": patch}.
func (c *Client) UpdateProduct(ctx context.Context, id int, patch map[string]any) (*Product, error) {
	payload, err := json.Marshal(map[string]any{"product": patch})
	if err != nil {
		return nil, &ResourceError{Err: err}
	}
	raw, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/admin/products/%d.json", id), payload)
	if err != nil {
		return nil, err
	}
	return decodeProduct(raw)
}

func decodeProduct(raw []byte) (*Product, error) {
	var product Product
	if err := json.Unmarshal(raw, &product); err != nil {
		return nil, &ResourceError{Err: fmt.Errorf("expected product object: %w", err)}
	}
	if product.ID == 0 {
		return nil, &ResourceError{Err: fmt.Errorf("missing product.id")}
	}
	return &product, nil
}
