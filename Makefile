SPEC_URL := https://docs.alpaca.markets/us/openapi/market-data-api.json
SPEC_FILE := gen/oapi/market-data-api.json

.PHONY: fetch-spec gen-oapi test run otel-smoke

fetch-spec:
	curl -fsSL --remove-on-error $(SPEC_URL) -o $(SPEC_FILE)

gen-oapi: fetch-spec
	go -C tools run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config ../gen/oapi/codegen.cfg.yml -o ../gen/oapi/server-oapi.gen.go ../$(SPEC_FILE)

test:
	go test ./... -race

run:
	go run .

# End-to-end telemetry smoke test: boots the server with stdout OTel exporters,
# drives a request + the poller, and shows one trace id threading through the
# logs, metrics, and traces. Requires python3 and jq.
otel-smoke:
	./scripts/otel-correlation-smoke.sh
