.PHONY: help doctor test ci build api web fixture demo bench questions schema seed redteam tidy clean

help:
	@echo "ARGUS"
	@echo ""
	@echo "  fixture   run the API on recorded data - no database, no API key"
	@echo "  demo      docker compose up, seed the graph, start API + UI"
	@echo "  doctor    verify a FalkorDB instance is usable by ARGUS"
	@echo "  schema    create indexes and constraints only"
	@echo "  seed      ingest the synthetic corpus"
	@echo "  bench     run every retrieval arm, write bench/results.md"
	@echo "  questions write the benchmark question set to bench/questions.json"
	@echo "  test      run the Go test suite"
	@echo "  ci        everything CI runs: gofmt, vet, go test, biome, tsc, build"
	@echo "  redteam   run the adversarial suite"
	@echo "  build     build all binaries into ./bin"

doctor:
	go run ./cmd/argus-doctor

test:
	go test ./...

# One command that runs exactly what CI runs, so a green local run means a
# green pipeline. The gofmt check is a diff-empty assertion rather than a
# rewrite: CI must fail on unformatted code, not silently fix it.
ci:
	@echo "==> gofmt"
	@test -z "$$(gofmt -l ./cmd ./internal)" || (gofmt -l ./cmd ./internal && echo "unformatted files above" && exit 1)
	@echo "==> go vet"
	go vet ./...
	@echo "==> go test"
	go test ./...
	@echo "==> web"
	cd web && npm run ci

build:
	go build -o bin/ ./cmd/...

api:
	go run ./cmd/argus-api

fixture:
	@echo "serving recorded data on :8080 - run 'make web' in another shell"
	go run ./cmd/argus-api -fixture

web:
	cd web && npm install --silent && npm run dev

schema:
	go run ./cmd/argus-ingest -schema-only

seed:
	go run ./cmd/argus-ingest -synthetic -reset

questions:
	go run ./cmd/argus-ingest -questions bench/questions.json

demo:
	docker compose up -d
	@echo "waiting for FalkorDB..."
	@sleep 5
	go run ./cmd/argus-doctor
	go run ./cmd/argus-ingest -synthetic -reset
	@echo ""
	@echo "graph seeded. now run:  make api   (and  make web  in another shell)"

bench:
	go run ./cmd/argus-bench -out bench/results.md -json bench/raw.json

redteam:
	go test ./internal/kg/ ./internal/extract/ -run "Injection|Poison|Fork|Validate_Rejects|WriteClause" -v

tidy:
	go mod tidy && gofmt -w ./cmd ./internal

clean:
	rm -rf bin web/.next
