package insales_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/testutil"
)

func TestOrdersAndProductsAgainstSimulator(t *testing.T) {
	sim, err := testutil.NewPlatformSimulator("app", "secret", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()

	sim.Define(http.MethodGet, "/admin/orders.json", testutil.Response{
		Status: 200, Body: `[{"id":5,"number":100,"status":"new"}]`,
		Headers: map[string]string{"API-Usage-Limit": "2/500"},
	})
	sim.Define(http.MethodGet, "/admin/orders/5.json", testutil.Response{Status: 200, Body: `{"id":5,"number":100}`})
	sim.Define(http.MethodGet, "/admin/orders/count.json", testutil.Response{Status: 200, Body: `{"count":3}`})
	sim.Define(http.MethodPut, "/admin/orders/5.json", testutil.Response{Status: 200, Body: `{"id":5,"number":100,"status":"cancelled"}`})
	sim.Define(http.MethodGet, "/admin/products.json", testutil.Response{Status: 200, Body: `[{"id":9,"title":"Mug"}]`})
	sim.Define(http.MethodGet, "/admin/products/9.json", testutil.Response{Status: 200, Body: `{"id":9,"title":"Mug"}`})
	sim.Define(http.MethodGet, "/admin/products/count.json", testutil.Response{Status: 200, Body: `{"count":1}`})
	sim.Define(http.MethodPut, "/admin/products/9.json", testutil.Response{Status: 200, Body: `{"id":9,"title":"Cup"}`})

	client := insales.NewClient(sim.ShopURL(), "app", "pw")
	ctx := context.Background()

	orders, err := client.ListOrders(ctx, 1, 10)
	if err != nil || len(orders) != 1 || orders[0].ID != 5 || orders[0].Number != 100 {
		t.Fatalf("orders %+v %v", orders, err)
	}
	if client.UsageLimit() != "2/500" {
		t.Fatalf("usage %q", client.UsageLimit())
	}
	order, err := client.GetOrder(ctx, 5)
	if err != nil || order.Number != 100 {
		t.Fatalf("order %+v %v", order, err)
	}
	count, err := client.CountOrders(ctx)
	if err != nil || count != 3 {
		t.Fatalf("count %d %v", count, err)
	}
	updated, err := client.UpdateOrder(ctx, 5, map[string]any{"status": "cancelled"})
	if err != nil || updated.Status != "cancelled" {
		t.Fatalf("update %+v %v", updated, err)
	}

	products, err := client.ListProducts(ctx, 1, 10)
	if err != nil || products[0].Title != "Mug" {
		t.Fatalf("products %+v %v", products, err)
	}
	product, err := client.GetProduct(ctx, 9)
	if err != nil || product.ID != 9 {
		t.Fatal(err)
	}
	if n, err := client.CountProducts(ctx); err != nil || n != 1 {
		t.Fatalf("product count %d %v", n, err)
	}
	renamed, err := client.UpdateProduct(ctx, 9, map[string]any{"title": "Cup"})
	if err != nil || renamed.Title != "Cup" {
		t.Fatalf("product update %+v %v", renamed, err)
	}
}

func TestUpdateOrderSendsJSON(t *testing.T) {
	sim, err := testutil.NewPlatformSimulator("app", "secret", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	var body []byte
	sim.Define(http.MethodPut, "/admin/orders/5.json", testutil.Response{Status: 200, Body: `{"id":5,"number":1}`})
	client := insales.NewClient(sim.ShopURL(), "app", "pw")
	client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return http.DefaultTransport.RoundTrip(r)
	})})
	if _, err := client.UpdateOrder(context.Background(), 5, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"order":{"status":"cancelled"}}` {
		t.Fatalf("body %s", body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
