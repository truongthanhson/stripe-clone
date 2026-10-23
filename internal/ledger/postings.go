package ledger

// Fee schedule: 2.9% plus a fixed amount per currency, in minor units (Stripe's US card pricing).
const feeBasisPoints = 290

var fixedFees = map[string]int64{"usd": 30, "eur": 25, "gbp": 20, "jpy": 30, "vnd": 2000}

// Fee is what the platform keeps from a charge of amount. Rounded half up; never more than amount.
func Fee(currency string, amount int64) int64 {
	fee := (amount*feeBasisPoints+5_000)/10_000 + fixedFees[currency]
	return min(fee, amount)
}

// Charge is the posting for captured money: the network owes us the gross amount; we owe the merchant
// the net and keep the fee.
//
//	debit  network_receivable  amount
//	credit merchant pending    amount - fee
//	credit platform fees       fee
func Charge(paymentIntentID string, merchantID int64, livemode bool, currency string, amount int64) Transaction {
	fee := Fee(currency, amount)
	entries := []Entry{{NetworkReceivable(livemode, currency), amount}}
	// Either side can be zero (a free charge, or one the fee swallows); a zero entry is not allowed.
	if net := amount - fee; net > 0 {
		entries = append(entries, Entry{MerchantPending(merchantID, livemode, currency), -net})
	}
	if fee > 0 {
		entries = append(entries, Entry{PlatformFees(livemode, currency), -fee})
	}
	return Transaction{
		Type: "charge", SourceID: paymentIntentID, MerchantID: merchantID, Livemode: livemode,
		Currency: currency, Description: "Payment " + paymentIntentID, Entries: entries,
	}
}

// Refund is the posting for money returned to a customer. The merchant's balance pays for it;
// the fee from the original charge is not returned (Stripe's policy too).
//
//	debit  merchant pending    amount
//	credit network_receivable  amount
func Refund(refundID string, merchantID int64, livemode bool, currency string, amount int64) Transaction {
	return Transaction{
		Type: "refund", SourceID: refundID, MerchantID: merchantID, Livemode: livemode,
		Currency: currency, Description: "Refund " + refundID,
		Entries: []Entry{
			{MerchantPending(merchantID, livemode, currency), amount},
			{NetworkReceivable(livemode, currency), -amount},
		},
	}
}
