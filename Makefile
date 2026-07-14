SPEC_URL := https://docs.alpaca.markets/us/openapi/market-data-api.json
SPEC_FILE := gen/oapi/market-data-api.json

.PHONY: fetch-spec gen-oapi test run run-app otel-smoke signoz-up signoz-down signoz-logs signoz-setup signoz-wait

fetch-spec:
	curl -fsSL --remove-on-error $(SPEC_URL) -o $(SPEC_FILE)

gen-oapi: fetch-spec
	go -C tools run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config ../gen/oapi/codegen.cfg.yml -o ../gen/oapi/server-oapi.gen.go ../$(SPEC_FILE)

test:
	go test ./... -race

# Full local stack: start SigNoz (idempotent), ensure it is set up and its OTLP
# receiver is ready, then run the app (on :8080) shipping telemetry to SigNoz.
run: signoz-up signoz-setup signoz-wait
	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 go run .

# Run only the application, without SigNoz. Telemetry goes to stdout unless
# OTEL_EXPORTER_OTLP_ENDPOINT is set in your .env.
run-app:
	go run .

# End-to-end telemetry smoke test: boots the server with stdout OTel exporters,
# drives a request + the poller, and shows one trace id threading through the
# logs, metrics, and traces. Requires python3 and jq.
otel-smoke:
	./scripts/otel-correlation-smoke.sh

# --- Local SigNoz (OTel-native observability backend) — see deploy/signoz/README.md ---
signoz-up:
	cd deploy/signoz && docker compose up -d

signoz-down:
	cd deploy/signoz && docker compose down

signoz-logs:
	cd deploy/signoz && docker compose logs -f --tail 100

# REQUIRED once after signoz-up: creates the admin/org so the collector opens its OTLP receiver.
signoz-setup:
	./deploy/signoz/setup.sh

# Wait until the collector's OTLP receiver accepts data (it comes up ~30s after setup).
signoz-wait:
	@echo "==> waiting for SigNoz OTLP receiver on :4318 ..."
	@for i in $$(seq 1 30); do \
	  code=$$(curl -s -m 3 -o /dev/null -w '%{http_code}' -X POST http://localhost:4318/v1/traces \
	    -H 'Content-Type: application/json' -d '{"resourceSpans":[]}' 2>/dev/null); \
	  if [ "$$code" = "200" ]; then echo "==> OTLP ready"; break; fi; \
	  sleep 3; \
	done
