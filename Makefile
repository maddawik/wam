.PHONY: build run test

build:
	@go build -o wam main.go

run:
	@go run main.go

test:
	@go test ./...
