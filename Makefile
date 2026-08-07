.PHONY: build test vet run compose-up compose-down vendor tidy

build:
	go build ./...

vet:
	go vet ./...

# Testes de integração exigem TEST_DATABASE_URL e compartilham 1 Postgres (-p 1).
test:
	go test -p 1 ./...

run:
	go run ./cmd/server

tidy:
	go mod tidy

vendor:
	go mod vendor

compose-up:
	docker compose up --build

compose-down:
	docker compose down -v
