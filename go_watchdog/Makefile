.PHONY: build build-amd64 test clean

build:
	go build -o go_watchdog .

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o go_watchdog .

test:
	go test ./...

clean:
	rm -f go_watchdog
