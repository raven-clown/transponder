.PHONY: check test lint cover

check: lint cover

test:
	go test -race ./...

lint:
	golangci-lint run ./...

cover:
	go test -race -coverprofile=cover.out -covermode=atomic ./...
	go run github.com/vladopajic/go-test-coverage/v2@v2.21.0 --config=.testcoverage.yml
