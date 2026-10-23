package ledger

import "fmt"

func modeName(livemode bool) string {
	if livemode {
		return "live"
	}
	return "test"
}

// MerchantPending is what the platform owes a merchant for charges not yet paid out.
func MerchantPending(merchantID int64, livemode bool, currency string) Account {
	return Account{
		Code:       fmt.Sprintf("merchant:%d:%s:%s:pending", merchantID, modeName(livemode), currency),
		MerchantID: &merchantID, Livemode: livemode, Currency: currency, Type: Liability,
	}
}

// NetworkReceivable is what the card network owes the platform for captured charges, less refunds.
func NetworkReceivable(livemode bool, currency string) Account {
	return Account{
		Code:     fmt.Sprintf("platform:%s:%s:network_receivable", modeName(livemode), currency),
		Livemode: livemode, Currency: currency, Type: Asset,
	}
}

// PlatformFees is the platform's fee revenue.
func PlatformFees(livemode bool, currency string) Account {
	return Account{
		Code:     fmt.Sprintf("platform:%s:%s:fees", modeName(livemode), currency),
		Livemode: livemode, Currency: currency, Type: Revenue,
	}
}
