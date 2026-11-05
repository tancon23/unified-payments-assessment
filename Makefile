TEST_DATABASE_URL ?= postgres://paygate:paygate@localhost:55432/paygate?sslmode=disable
STRIPE_CLI_IMAGE  := stripe/stripe-cli:v1.54.0
LIVE              := docker compose -f docker-compose.yml -f docker-compose.live.yml
LOAD_ENV          := set -a && . ./.env && set +a

.PHONY: up live down migrate seed test test-unit test-live stripe-webhook-secret vet

up: ## Start Postgres, the local provider stand-in, the api and the worker
	docker compose up --build

live: ## Start Postgres, the api and the worker against the real Stripe and PayPal sandboxes (needs .env)
	$(LIVE) up --build

down: ## Stop everything, in either mode, and delete the database volume
	docker compose down -v --remove-orphans

migrate: ## Apply database migrations
	docker compose up -d --wait db
	docker compose run --rm --no-deps --build api migrate

seed: ## Create the acme and globex tenants and print new API keys
	docker compose up -d --wait db
	docker compose run --rm --no-deps --build api seed

test: ## Run every test, starting Postgres first
	docker compose up -d --wait db
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -count=1 ./...

test-unit: ## Run the tests that need no database or credentials
	go test -race -count=1 ./...

test-live: ## Check the Stripe and PayPal adapters against the real sandboxes (needs .env)
	$(LOAD_ENV) && go test -count=1 -v -run Live ./internal/provider/...

stripe-webhook-secret: ## Print the signing secret for events the Stripe CLI forwards (needs .env)
	$(LOAD_ENV) && STRIPE_API_KEY="$$STRIPE_SECRET_KEY" docker run --rm -e STRIPE_API_KEY $(STRIPE_CLI_IMAGE) listen --print-secret

vet:
	go vet ./...
