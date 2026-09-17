BIN=fleet
VPS?=fleet@your-hetzner-host

build:        ; go build -o bin/$(BIN) ./cmd/fleet
linux:        ; GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BIN)-linux-amd64 ./cmd/fleet
darwin:       ; GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BIN)-darwin-arm64 ./cmd/fleet
dist:         linux darwin
test:         ; go test ./...
lint:         ; go vet ./...
install:      build ; install -m 0755 bin/$(BIN) $(HOME)/.local/bin/$(BIN)
deploy:       linux ; ssh $(VPS) 'mkdir -p ~/.local/bin' && scp dist/$(BIN)-linux-amd64 $(VPS):~/.local/bin/$(BIN) && ssh $(VPS) 'chmod +x ~/.local/bin/fleet && ~/.local/bin/fleet version'
release:      dist ; gh release create $(TAG) dist/$(BIN)-linux-amd64 dist/$(BIN)-darwin-arm64 --title $(TAG) --notes "fleet $(TAG)"
