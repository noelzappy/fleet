BIN=fleet
VPS?=fleet@your-hetzner-host

build:        ; go build -ldflags "-X github.com/noelzappy/fleet/internal/cli.version=$$(git describe --tags --always --dirty)" -o bin/$(BIN) ./cmd/fleet
linux:        ; GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BIN)-linux-amd64 ./cmd/fleet
darwin:       ; GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BIN)-darwin-arm64 ./cmd/fleet
dist:         linux darwin
test:         ; go test ./...
lint:         ; go vet ./...
install:      build ; install -m 0755 bin/$(BIN) $(HOME)/.local/bin/$(BIN)
deploy:       linux ; ssh $(VPS) 'mkdir -p ~/.local/bin' && scp dist/$(BIN)-linux-amd64 $(VPS):~/.local/bin/.$(BIN).new && ssh $(VPS) 'chmod +x ~/.local/bin/.fleet.new && mv -f ~/.local/bin/.fleet.new ~/.local/bin/fleet && ~/.local/bin/fleet version'
release:      ; git tag $(TAG) && git push origin $(TAG)   # CI (goreleaser) builds, publishes and updates the Homebrew tap
snapshot:     ; goreleaser release --snapshot --clean
