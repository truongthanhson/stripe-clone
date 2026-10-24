# Design note: hosted checkout

A merchant creates a Checkout Session through the API and sends the buyer to its `url`, a payment page
this server renders. The buyer types a card, pays, and is redirected to the merchant's `success_url`.
This is how a shop with no backend card handling of its own takes a payment.
**Gate:** 20 concurrent submits of the same page charge the card once, and an expiry racing a
payment never leaves an expired session with money taken (`TestE2ECheckoutGate` in
`cmd/api/checkout_test.go`). It passes.

## The model

```
POST /v1/checkout/sessions ──▶ session (open) + PaymentIntent (requires_payment_method), one transaction
                                     │
buyer opens /c/pay/cs_test_...  ─────┤
buyer submits the card form ──▶ PaymentIntent.Confirm ──▶ succeeded ──▶ session complete ──303──▶ success_url
                                     │                  └─ declined ──▶ session stays open, page shows why
                                     │                  └─ timeout  ──▶ processing, page refreshes itself
expire (API, or expires_at passed) ──▶ PaymentIntent.Cancel ──▶ canceled ──▶ session expired
```

A session owns exactly one PaymentIntent, created with it. **The PaymentIntent decides every race.**
Its status moves by compare-and-swap (Phase 1), so of a confirm and a cancel that arrive together,
exactly one wins. The session's own `status` only records how the PaymentIntent ended
(succeeded → complete, canceled → expired) and is re-derived on every read, so if the server dies
between updating the PaymentIntent and the session, the next read repairs it.

## Decisions

| Decision | Why | Alternative rejected |
|---|---|---|
| Session status follows its PaymentIntent, re-derived on read | One source of truth; a crash between two updates heals itself | Two independent state machines: they disagree after the first crash |
| Expire cancels the PaymentIntent first, then marks the session | If a payment already claimed it, the cancel loses and nothing is expired | Mark the session first: a payment in flight completes on an expired session |
| Create the PaymentIntent with the session, in one transaction | The merchant gets `payment_intent` immediately; no orphan rows either way | Create it lazily at the first submit: one more race (two first submits) to handle |
| Lazy expiry on read, no sweeper job | Correct without a background process; an unread expired session harms nobody | A cron sweep: needed later only for `checkout.session.expired` webhooks (Phase 3) |
| Post/Redirect/Get after every recorded outcome | Reloading the result page never resubmits a card | Render the result from the POST: F5 asks to resend the form |
| No JavaScript on the page | Works everywhere, no client state to get wrong, strict CSP (`default-src 'none'`) | htmx: nothing on this page needs partial updates; the dashboard uses it |
| Only test card numbers accepted, mapped to `pm_card_*` | Real numbers never reach the network or a log; PCI scope stays zero | Store a PaymentMethod row: no value until a real processor tokenizes cards |
| At most 10 card attempts per session | A public page that answers "declined" is a card-testing oracle | No limit: attackers try stolen numbers one by one; IP rate limits come with Redis |
| The session ID is the page's only credential | 128 random bits, like Stripe's; no cookies, so no CSRF surface | Signed links with expiry: same strength, more moving parts |
| `success_url`/`cancel_url` must be absolute http(s) | They become `href`s and redirects; `javascript:` would run on our origin | Accept any string: stored XSS on the payment page |

## Failure cases and what happens

| Failure | Result | Proven by |
|---|---|---|
| Buyer double-clicks Pay (or 20 tabs submit) | One charge; every submit is redirected to the same outcome | `TestE2ECheckoutGate` |
| Expiry races a payment | Either complete with the money, or expired with none | `TestE2ECheckoutGate` |
| Card declined | Session stays open; the page shows the decline; another card can pay | `TestE2ECheckoutPage` |
| Typo, expired card date, real card number | 422 with the field marked; the network is never called | `TestE2ECheckoutPage` |
| Bank never answers | Page says "processing" and refreshes; the session can't be expired by API or by time | `TestE2ECheckoutExpiry` |
| Session past `expires_at` | Expired on the next read; paying it charges nothing | `TestE2ECheckoutExpiry` |
| Card testing through the page | 11th attempt refused before the network | `TestE2ECheckoutAttemptsLimit` |
| Another merchant or the other mode reads a session | 404 | `TestE2ECheckoutCreate` |
| Invalid session request | 400 naming the parameter; no PaymentIntent left behind | `TestE2ECheckoutCreateErrors` |
| Page framed by another site | Refused (`frame-ancestors 'none'`, `X-Frame-Options: DENY`) | `TestE2ECheckoutPage` |

## Known gaps, scheduled

- **A session left `processing` stays open** until Phase 3's resolver settles its PaymentIntent; the
  next read then completes or reopens it on its own.
- **No `checkout.session.completed` event yet.** Merchants poll the session or rely on the redirect,
  which a buyer can close before it lands. Phase 3 adds events and webhooks; until then do not
  fulfil orders from the redirect alone.
- **Live mode** shows the page but refuses to charge, like the API, until the processor adapter exists.
