package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/listing"
)

type balanceStore interface {
	Balances(ctx context.Context, merchantID int64, livemode bool) ([]ledger.Balance, error)
	GetTransaction(ctx context.Context, merchantID int64, livemode bool, id string) (ledger.BalanceTransaction, error)
	ListTransactions(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]ledger.BalanceTransaction, bool, error)
}

type moneyJSON struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type balanceJSON struct {
	Object    string      `json:"object"`
	Available []moneyJSON `json:"available"`
	Livemode  bool        `json:"livemode"`
	Pending   []moneyJSON `json:"pending"`
}

func handleGetBalance(store balanceStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		balances, err := store.Balances(r.Context(), p.MerchantID, p.Livemode())
		if err != nil {
			writeInternalError(w, r, "get balance", err)
			return
		}
		out := balanceJSON{Object: "balance", Livemode: p.Livemode(), Available: []moneyJSON{}, Pending: []moneyJSON{}}
		for _, b := range balances {
			out.Available = append(out.Available, moneyJSON{b.Available, b.Currency})
			out.Pending = append(out.Pending, moneyJSON{b.Pending, b.Currency})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type balanceTransactionJSON struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Amount      int64  `json:"amount"`
	Created     int64  `json:"created"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	Fee         int64  `json:"fee"`
	Net         int64  `json:"net"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	Type        string `json:"type"`
}

func renderBalanceTransaction(bt ledger.BalanceTransaction) balanceTransactionJSON {
	return balanceTransactionJSON{
		ID: bt.ID, Object: "balance_transaction", Amount: bt.Amount, Created: bt.Created.Unix(),
		Currency: bt.Currency, Description: bt.Description, Fee: bt.Fee, Net: bt.Net, Source: bt.Source,
		Status: "pending", // becomes available once payouts settle funds (Phase 4)
		Type:   bt.Type,
	}
}

func noSuchBalanceTransaction(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "id",
		Message: "No such balance_transaction: '" + id + "'"}
}

func handleGetBalanceTransaction(store balanceStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		bt, err := store.GetTransaction(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		switch {
		case errors.Is(err, ledger.ErrNotFound):
			writeError(w, http.StatusNotFound, noSuchBalanceTransaction(r.PathValue("id")))
		case err != nil:
			writeInternalError(w, r, "get balance transaction", err)
		default:
			writeJSON(w, http.StatusOK, renderBalanceTransaction(bt))
		}
	}
}

func handleListBalanceTransactions(store balanceStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.ListTransactions(r.Context(), p.MerchantID, p.Livemode(), params)
		if errors.Is(err, ledger.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchBalanceTransaction(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeInternalError(w, r, "list balance transactions", err)
			return
		}
		out := listJSON[balanceTransactionJSON]{Object: "list", URL: "/v1/balance_transactions", HasMore: hasMore, Data: []balanceTransactionJSON{}}
		for _, bt := range page {
			out.Data = append(out.Data, renderBalanceTransaction(bt))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
