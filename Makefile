GO ?= go
.PHONY: build test test-db vet fmt eval-fixtures eval score run
build: ; $(GO) build -o bin/incidentd ./cmd/incidentd
test: ; $(GO) test ./... && python3 -m pytest -q eval/tests
test-db: ; TEST_DATABASE_URL=$${TEST_DATABASE_URL:?set TEST_DATABASE_URL} $(GO) test ./db/...
vet: ; $(GO) vet ./...
fmt: ; gofmt -w .
eval-fixtures: ; python3 eval/gen_synthetic.py
eval: ; $(GO) run ./cmd/incidentd eval run -out eval/out/run.jsonl
score: ; python3 eval/score.py eval/out/run.jsonl --report eval/out/report.md
run: ; $(GO) run ./cmd/incidentd serve -config $${CONFIG:-config/local.yaml}
