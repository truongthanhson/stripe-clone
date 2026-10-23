package main

import (
	"net/url"
	"testing"
)

func TestE2ECustomers(t *testing.T) {
	e := newE2E(t)
	post := func(t *testing.T, path string, form url.Values) (int, map[string]any) {
		t.Helper()
		rec := e.post(t, path, e.testKey, form, nil)
		return rec.Code, decode(t, rec.Body.Bytes())
	}

	code, jenny := post(t, "/v1/customers", url.Values{
		"email": {"Jenny.Rosen@example.com"}, "name": {"Jenny Rosen"}, "description": {"VIP"}, "metadata[crm_id]": {"42"},
	})
	if code != 200 || jenny["object"] != "customer" || jenny["email"] != "Jenny.Rosen@example.com" || jenny["livemode"] != false {
		t.Fatalf("create: %d %v", code, jenny)
	}
	id := jenny["id"].(string)
	path := "/v1/customers/" + id

	t.Run("update sets, clears and merges", func(t *testing.T) {
		code, c := post(t, path, url.Values{"name": {"Jenny R."}, "description": {""}, "metadata[tier]": {"gold"}})
		md, _ := c["metadata"].(map[string]any)
		if code != 200 || c["name"] != "Jenny R." || c["description"] != nil || c["email"] != "Jenny.Rosen@example.com" ||
			md["crm_id"] != "42" || md["tier"] != "gold" {
			t.Fatalf("update: %d %v", code, c)
		}
	})

	t.Run("invalid email", func(t *testing.T) {
		code, body := post(t, "/v1/customers", url.Values{"email": {"not-an-email"}})
		if e := errorOf(body); code != 400 || e["param"] != "email" {
			t.Fatalf("got %d %v", code, body)
		}
	})

	t.Run("list filters by email, case-insensitively", func(t *testing.T) {
		post(t, "/v1/customers", url.Values{"email": {"someone@else.test"}})
		_, list := e.do(t, "GET", "/v1/customers?email=jenny.rosen@EXAMPLE.com", e.testKey, nil)
		data := list["data"].([]any)
		if len(data) != 1 || data[0].(map[string]any)["id"] != id {
			t.Fatalf("filtered list = %v", list)
		}
		_, all := e.do(t, "GET", "/v1/customers", e.testKey, nil)
		if len(all["data"].([]any)) != 2 {
			t.Fatalf("list = %v", all)
		}
	})

	t.Run("payments belong to a customer", func(t *testing.T) {
		pi := e.create(t, url.Values{"customer": {id}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		e.create(t, url.Values{})
		_, list := e.do(t, "GET", "/v1/payment_intents?customer="+id, e.testKey, nil)
		data := list["data"].([]any)
		if len(data) != 1 || data[0].(map[string]any)["id"] != pi.ID || data[0].(map[string]any)["customer"] != id {
			t.Fatalf("customer's payments = %v", list)
		}

		_, other := e.do(t, "POST", "/v1/customers", e.otherMerchantKey, url.Values{"name": {"Not yours"}})
		for _, foreign := range []string{other["id"].(string), "cus_missing"} {
			code, body := e.do(t, "POST", "/v1/payment_intents", e.testKey, url.Values{"amount": {"2000"}, "currency": {"usd"}, "customer": {foreign}})
			if e := errorOf(body); code != 400 || e["code"] != "resource_missing" || e["param"] != "customer" {
				t.Fatalf("customer %s: %d %v", foreign, code, body)
			}
		}
	})

	t.Run("other merchants and modes cannot see the customer", func(t *testing.T) {
		for _, key := range []string{e.liveKey, e.otherMerchantKey} {
			if code, _ := e.do(t, "GET", path, key, nil); code != 404 {
				t.Fatalf("get with foreign key: %d", code)
			}
		}
	})

	t.Run("delete hides the customer but keeps its payments", func(t *testing.T) {
		code, del := e.do(t, "DELETE", path, e.testKey, nil)
		if code != 200 || del["deleted"] != true || del["id"] != id {
			t.Fatalf("delete: %d %v", code, del)
		}
		if code, got := e.do(t, "GET", path, e.testKey, nil); code != 200 || got["deleted"] != true || got["email"] != nil {
			t.Fatalf("get deleted: %d %v", code, got)
		}
		if code, _ := e.do(t, "DELETE", path, e.testKey, nil); code != 404 {
			t.Fatalf("delete twice: %d", code)
		}
		if code, _ := post(t, path, url.Values{"name": {"x"}}); code != 404 {
			t.Fatalf("update deleted: %d", code)
		}
		_, list := e.do(t, "GET", "/v1/customers", e.testKey, nil)
		if len(list["data"].([]any)) != 1 {
			t.Fatalf("deleted customer still listed: %v", list)
		}
		_, payments := e.do(t, "GET", "/v1/payment_intents?customer="+id, e.testKey, nil)
		if len(payments["data"].([]any)) != 1 {
			t.Fatal("payments of a deleted customer disappeared")
		}
		code, body := e.do(t, "POST", "/v1/payment_intents", e.testKey, url.Values{"amount": {"2000"}, "currency": {"usd"}, "customer": {id}})
		if code != 400 || errorOf(body)["param"] != "customer" {
			t.Fatalf("new payment for a deleted customer: %d %v", code, body)
		}
	})
}
