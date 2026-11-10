.PHONY: up down logs test test-go build fmt

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f

test: test-go

test-go:
	go mod download && go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/svma ./cmd/svma

fmt:
	gofmt -w $$(find . -name '*.go')
