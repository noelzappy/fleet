BIN=fleet
VPS?=fleet@your-hetzner-host

build:        ; go build -o bin/$(BIN) ./cmd/fleet
linux:        ; GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BIN)-linux-amd64 ./cmd/fleet
test:         ; go test ./...
lint:         ; go vet ./...
install:      build ; install -m 0755 bin/$(BIN) $(HOME)/.local/bin/$(BIN)
deploy:       linux ; scp dist/$(BIN)-linux-amd64 $(VPS):~/.local/bin/$(BIN) && ssh $(VPS) 'chmod +x ~/.local/bin/fleet && fleet version'
release:      linux ; gh release create $(TAG) dist/$(BIN)-linux-amd64 --title $(TAG) --notes "fleet $(TAG)"
