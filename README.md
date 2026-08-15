# Shuttle Court

Shuttle Court is a two-party mediation service. The JavaScript adapter receives Caspian channel events; the Go service owns all mediation state and PostgreSQL persistence.

## Run locally

1. Create a PostgreSQL database and apply `schema.sql`.
2. Copy `.env.example` to `.env` and fill the required database, OpenRouter, and Caspian values.
3. Start the Go engine with `go run .`.
4. In `channel-adapter`, run `npm install` and then `node index.js`.

The engine listens on port 8080 and the adapter's authenticated proactive-delivery endpoint listens on port 8081 by default. `ENGINE_TOKEN` and `ADAPTER_TOKEN` should be different random secrets.

Existing databases receive additive compatibility migrations on Go service startup. `schema.sql` remains the canonical clean-install schema.

## Verify

```sh
GOCACHE=/tmp/shuttle-court-go-cache go test -race ./...
GOCACHE=/tmp/shuttle-court-go-cache go vet ./...
node --check channel-adapter/index.js
```

The live mediation script requires running PostgreSQL, Go, adapter, OpenRouter, and Caspian services:

```sh
node channel-adapter/test-mediation-flow.js
```
