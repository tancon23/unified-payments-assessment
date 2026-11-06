# paygate

A multi-tenant payments gateway in Go. Internal services talk to one HTTP API; paygate talks to Stripe and PayPal. It opens hosted checkouts (one-off and subscription), takes refunds, and turns provider webhooks into payment status changes exactly once. A worker finishes anything a request or a webhook left open.

## Start it

```sh
make up      # docker compose up --build
make seed    # in a second terminal: creates the acme and globex tenants and prints their API keys
```

`make up` starts four containers:

| Service   | Port  | What it is                                                                                     |
|-----------|-------|------------------------------------------------------------------------------------------------|
| `api`     | 8080  | The HTTP API. Applies migrations on start.                                                     |
| `worker`  |       | Retries provider calls that got no answer and checks open checkouts and refunds with the provider. |
| `db`      | 55432 | PostgreSQL 17.                                                                                 |
| `sandbox` | 8090  | A local stand-in for the Stripe and PayPal sandbox APIs. It also sends their webhooks.         |

`make seed` prints a new key for each tenant every time it runs; only SHA-256 hashes are stored. Keep the acme key handy:

```sh
export API_KEY=acme_...
```

To run against real Stripe and PayPal sandbox accounts instead, see [Live sandboxes](#live-sandboxes). `make down` stops everything, in either mode, and deletes the database.

## Try it

Open a checkout:

```sh
curl -s localhost:8080/v1/checkouts \
  -H "X-API-Key: $API_KEY" \
  -H 'Idempotency-Key: order-1001' \
  -d '{
        "provider": "stripe",
        "mode": "payment",
        "amount_minor": 2500,
        "currency": "usd",
        "description": "Order 1001",
        "success_url": "https://shop.example/thanks",
        "cancel_url": "https://shop.example/cart"
      }'
```

The response holds the payment `id`, the `checkout_url` to send the customer to, and `provider_checkout_id`. Sending the same request again with the same `Idempotency-Key` returns the same payment with `Idempotent-Replayed: true`.

Play the customer paying. The sandbox delivers the signed webhook twice, as providers sometimes do:

```sh
curl -s -X POST 'localhost:8090/sandbox/checkouts/<provider_checkout_id>/pay?deliveries=2'
```

Each delivery's response from the API is echoed back: the first is `applied`, the second `duplicate`.

Read the payment, then refund part of it:

```sh
curl -s localhost:8080/v1/payments/<id> -H "X-API-Key: $API_KEY"

curl -s localhost:8080/v1/payments/<id>/refunds \
  -H "X-API-Key: $API_KEY" \
  -H 'Idempotency-Key: refund-1001' \
  -d '{"amount_minor": 1000}'
```

The other tenant gets `404` for the same payment.

The same flow works with `"provider": "paypal"`: once the order is approved, the API captures it and the payment goes to `succeeded`. For a subscription, send `"mode": "subscription"` with `"interval"`. PayPal subscriptions use one billing plan (see [Design decisions](#design-decisions)); the local sandbox's plan charges `2500` `usd` every `month`, so a PayPal subscription request must carry those values.

`docs/paygate.postman_collection.json` holds all of these requests, plus the sandbox controls below and the sample webhooks in `docs/webhooks/`.

## Fire test webhooks

### From the local sandbox

The sandbox plays both the customer and the provider's webhook sender. Every call returns what the API answered.

| Request | What happens |
|---|---|
| `POST /sandbox/checkouts/{ref}/pay?deliveries=N` | The customer pays. Stripe sends `checkout.session.completed`; PayPal sends `CHECKOUT.ORDER.APPROVED` (orders) or `BILLING.SUBSCRIPTION.ACTIVATED` (subscriptions). The event is delivered N times (1 to 5) with the same event id. |
| `POST /sandbox/checkouts/{ref}/fail` | Stripe sends `checkout.session.async_payment_failed`, and the payment fails. A PayPal order is approved but its capture is declined, so the payment fails. A PayPal subscription is cancelled, so the payment expires. |
| `POST /sandbox/checkouts/{ref}/expire` | The checkout expires. A PayPal order sends no webhook; the worker notices. |
| `POST /sandbox/faults` `{"provider":"stripe","kind":"unavailable","count":1}` | The next Stripe call answers 503. |
| `POST /sandbox/faults` `{"provider":"stripe","kind":"timeout","count":1}` | The next Stripe call is carried out, but the answer comes after the API has stopped waiting. |

With a fault set, opening a checkout returns `202` with status `unknown`. Within about a minute the worker asks again, with the same provider idempotency key, and the payment becomes `pending` with its checkout URL. The provider never opens a second checkout.

Refunds and PayPal captures also produce follow-up webhooks (`refund.created`, `PAYMENT.CAPTURE.COMPLETED`, `PAYMENT.CAPTURE.REFUNDED`) shortly afterwards. They arrive after the API call has recorded the result, so they are `ignored`.

A forged delivery is rejected with `400` and logged without its payload:

```sh
curl -s localhost:8080/v1/webhooks/stripe -H "Stripe-Signature: t=$(date +%s),v1=forged" -d '{"id":"evt_1"}'
```

Webhook outcomes are counted at `GET /metrics` as `paygate_webhooks_total{provider,outcome}`.

### From Stripe and PayPal

Against the live sandboxes, real webhooks arrive as described in [Live sandboxes](#live-sandboxes).

## Run the tests

```sh
make test
```

This starts the Postgres container and runs every test with the race detector. The service, store and HTTP tests each run against a fresh schema in a real database, because row locks and unique constraints are what keep payments correct.

- **`make test-unit`** runs without a database. Tests that need Postgres are skipped, which leaves the adapters, the contract suite, signature checks and money handling.
- **`make test-live`** runs the Stripe and PayPal adapters against the real sandboxes with the credentials in `.env`. It opens checkouts, pays and refunds a Stripe test payment, checks idempotency keys, reads the PayPal plan, and confirms that PayPal's verification API rejects a forged webhook. It refuses to run with a Stripe key that is not a test-mode key.

`internal/provider/providertest` is one contract suite that every adapter runs: Stripe and PayPal against the local sandbox, and the Paddle placeholder as a placeholder.

## API

Every tenant request carries `X-API-Key`. Every `POST` to a tenant route carries an `Idempotency-Key` (at most 255 characters), scoped to the tenant.

| Method and path | Body | Success |
|---|---|---|
| `POST /v1/checkouts` | `provider`, `mode` (`payment` or `subscription`), `amount_minor`, `currency`, `interval` (subscriptions), `description`, `success_url`, `cancel_url` | `201` new, `200` replayed, `202` provider gave no answer yet |
| `GET /v1/payments/{id}` | | `200`, with refunds |
| `POST /v1/payments/{id}/refunds` | `amount_minor` (omit to refund the rest) | `201` succeeded, `202` not confirmed yet, `200` replayed |
| `POST /v1/webhooks/{provider}` | Provider payload, signature-checked | `200` with the outcome |
| `GET /healthz`, `GET /metrics` | | `200` |

Payment statuses: `pending`, `unknown`, `authorized`, `succeeded`, `failed`, `expired`, `partially_refunded`, `refunded`. Refund statuses: `unknown`, `pending`, `succeeded`, `failed`. Payments and refunds also report `attempts`, `flagged` and `flag_reason`: an item the worker could not finish is flagged for a person instead of being marked failed.

Errors look like `{"error": {"code": "...", "message": "..."}}`:

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request`, `invalid_webhook` | Bad input, or a webhook that could not be verified. |
| 401 | `unauthenticated` | Missing or unknown API key. |
| 404 | `not_found`, `unknown_provider` | Absent, or owned by another tenant. |
| 409 | `request_in_progress`, `invalid_state` | The resource is being worked on right now, or the payment cannot be refunded. |
| 422 | `idempotency_key_reused`, `unsupported`, `provider_rejected` | The key was used for a different request; the operation is not offered (refunding a subscription); or the provider refused, in which case the payment or refund is included, marked `failed`. |
| 503 | `provider_unavailable` | A provider needed to verify a webhook did not answer; the provider redelivers. |

## Configuration

The api and the worker read the same variables.

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | (required) | PostgreSQL connection string. |
| `HTTP_ADDR` | `:8080` | API listen address. |
| `STRIPE_SECRET_KEY` | | Enables Stripe. |
| `STRIPE_WEBHOOK_SECRET` | | Signing secret of the Stripe webhook endpoint. Required with `STRIPE_SECRET_KEY`. |
| `STRIPE_API_BASE` | `https://api.stripe.com` | |
| `PAYPAL_CLIENT_ID`, `PAYPAL_CLIENT_SECRET` | | Enable PayPal. |
| `PAYPAL_WEBHOOK_ID` | | Id of the PayPal webhook registration. Without it, PayPal webhooks are rejected and the worker finds outcomes instead. |
| `PAYPAL_PLAN_ID` | | The billing plan PayPal subscriptions use. Without it, PayPal subscriptions are refused. |
| `PAYPAL_API_BASE` | `https://api-m.sandbox.paypal.com` | |
| `WORKER_INTERVAL` | `15s` | How often the worker runs. |
| `WORKER_FIRST_CHECK` | `15m` | How long before an open checkout or refund whose webhook has not arrived is checked with the provider (`1m` in Compose). |

## Live sandboxes

The same api and worker run against Stripe's test mode and the PayPal sandbox; only configuration changes.

1. Copy `.env.example` to `.env`. It is git-ignored.
2. **Stripe:** in the Dashboard, switch to test mode, open Developers > API keys, and put the secret key in `STRIPE_SECRET_KEY`. Then run `make stripe-webhook-secret` and put the printed `whsec_...` in `STRIPE_WEBHOOK_SECRET`.
3. **PayPal:** on developer.paypal.com, open Apps & Credentials > Sandbox, create an app, and put its client id and secret in `PAYPAL_CLIENT_ID` and `PAYPAL_CLIENT_SECRET`. Create one billing plan in the sandbox (a regular cycle charged every 1 day, week, month or year) and put its `P-...` id in `PAYPAL_PLAN_ID`. Register a webhook on the app and put its id in `PAYPAL_WEBHOOK_ID`.
4. Check the adapters: `make test-live`.
5. Start: `make live`, then `make seed`. This runs Postgres, the api and the worker (pointed at `api.stripe.com` and `api-m.sandbox.paypal.com`) and the Stripe CLI. The local stand-in does not start. Stripe credentials are required; PayPal is enabled once its credentials are set.

Open a checkout as in [Try it](#try-it), open its `checkout_url` in a browser, and pay:

- **Stripe:** card `4242 4242 4242 4242`, any future expiry, any CVC. The Stripe CLI container forwards the webhooks to the api.
- **PayPal:** log in with the sandbox personal account under Testing Tools > Sandbox Accounts. PayPal delivers webhooks only to a public URL, so the registered webhook must point at `https://<public-host>/v1/webhooks/paypal`, for example through a tunnel to port 8080. Without it, the worker notices the approval within `WORKER_FIRST_CHECK` and captures the order. PayPal's webhook simulator, or the samples in `docs/webhooks/paypal`, send test events to the registered URL.

## Design decisions

- **One API key per tenant.** Keys are stored as SHA-256 hashes in `tenants`. Several keys per tenant (per environment, or for rotation) would move the hash into an `api_keys` table keyed by tenant, with a label and a revoked-at time; the auth middleware would look the key up there instead.
- **Provider credentials in `.env`.** One set of Stripe and PayPal sandbox credentials serves every tenant. In production each tenant's provider credentials would live in AWS Secrets Manager, loaded by the adapter per tenant.
- **No answer is not a failure.** A provider call that times out or gets a 5xx leaves the payment or refund `unknown` and returns `202`. The worker retries with the same provider idempotency key, waiting 30 seconds and doubling the wait each time (1, 2, then 4 minutes). After 5 attempts in all it flags the item for a person. Nothing is marked `failed` unless the provider says so.
- **One PayPal billing plan.** PayPal subscriptions use the plan in `PAYPAL_PLAN_ID`, created once in the sandbox; paygate does not manage plans. A request whose amount, currency or interval differs from the plan is refused, so the gateway never records a price PayPal will not charge. Stripe subscriptions take their price from the request.

## Layout

```
cmd/api               HTTP API; `api migrate` and `api seed`
cmd/worker            retries and reconciliation
cmd/sandbox           local Stripe and PayPal stand-in
internal/gateway      provider-neutral contract: Provider, Capturer, events, errors
internal/provider     stripe, paypal, paddle (placeholder), shared HTTP error handling, contract suite
internal/payment      business rules: state machine, idempotency, refunds, webhooks, worker
internal/postgres     Store implementation and migrations
internal/httpapi      Gin routes, auth, error mapping, webhook metrics
internal/app          configuration and wiring shared by the api and the worker
internal/sandbox      the stand-in's implementation
docs                  Postman collection, sample webhooks
```

Adding a provider means writing one package that implements `gateway.Provider` (plus `gateway.Capturer` if it needs explicit capture), running the contract suite against it, and registering it in `internal/app`. Nothing in `internal/payment` changes.
