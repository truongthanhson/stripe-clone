package cardnetwork

// Card is what the network needs to charge. Only the fake ever sees a full number, and only a test one.
type Card struct {
	Number   string
	Brand    string
	ExpMonth int
	ExpYear  int
}

func (c Card) Last4() string { return c.Number[len(c.Number)-4:] }

// Test card numbers. The fake network decides the outcome from the number, so each
// failure a real bank produces can be triggered on purpose. The decline numbers are Stripe's.
const (
	CardVisa                = "4242424242424242"
	CardMastercard          = "5555555555554444"
	CardDeclined            = "4000000000000002"
	CardInsufficientFunds   = "4000000000009995"
	CardLostCard            = "4000000000009987"
	CardExpired             = "4000000000000069"
	CardIncorrectCVC        = "4000000000000127"
	CardProcessingError     = "4000000000000119"
	CardTimeoutAfterApprove = "4000000000009003" // approved by the network, but the answer never arrives
	CardRefundFails         = "4000000000005126" // charges fine; every refund is refused (Stripe's refund_fail card)
)

// testPaymentMethods are the payment method IDs merchants pass in test mode, named as Stripe's.
// The hosted checkout page maps a typed test card number to one of these IDs.
var testPaymentMethods = map[string]Card{
	"pm_card_visa":                            {Number: CardVisa, Brand: "visa"},
	"pm_card_mastercard":                      {Number: CardMastercard, Brand: "mastercard"},
	"pm_card_chargeDeclined":                  {Number: CardDeclined, Brand: "visa"},
	"pm_card_chargeDeclinedInsufficientFunds": {Number: CardInsufficientFunds, Brand: "visa"},
	"pm_card_chargeDeclinedLostCard":          {Number: CardLostCard, Brand: "visa"},
	"pm_card_chargeDeclinedExpiredCard":       {Number: CardExpired, Brand: "visa"},
	"pm_card_chargeDeclinedIncorrectCvc":      {Number: CardIncorrectCVC, Brand: "visa"},
	"pm_card_chargeDeclinedProcessingError":   {Number: CardProcessingError, Brand: "visa"},
	"pm_card_timeout":                         {Number: CardTimeoutAfterApprove, Brand: "visa"},
	"pm_card_refundFail":                      {Number: CardRefundFails, Brand: "visa"},
}

// TestPaymentMethodForNumber returns the test payment method ID for a test card number, so a card
// typed on the hosted checkout page becomes the same ID a merchant would pass from curl.
func TestPaymentMethodForNumber(number string) (string, bool) {
	for id, c := range testPaymentMethods {
		if c.Number == number {
			return id, true
		}
	}
	return "", false
}

// TestPaymentMethod returns the card behind a test payment method ID.
func TestPaymentMethod(id string) (Card, bool) {
	c, ok := testPaymentMethods[id]
	if ok {
		c.ExpMonth, c.ExpYear = 12, 2034
	}
	return c, ok
}
