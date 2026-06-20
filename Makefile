.PHONY: build install deploy push reasonix clean test vet

VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
BINARY := reasonix-telegram
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)
	systemctl daemon-reload
	systemctl enable --now reasonix-telegram
	@echo "tail logs: journalctl -u reasonix-telegram -f"

deploy: test vet build install
	@echo "=== deploy complete: built, tested, installed, restarted ==="

push:
	git add -A
	@echo "--- git status ---"
	git status --short
	@echo "---"
	@read -p "Commit message: " msg; \
		git commit -m "$$msg" && git push
	@echo "=== pushed ==="

reasonix:
	(cd ../reasonix && CGO_ENABLED=0 go build -o /usr/local/bin/reasonix ./cmd/reasonix)
	@reasonix --version

test:
	go test ./... -v -count=1

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
# Local overrides (e.g. pointing 'reasonix' at a fork). File is gitignored.
-include Makefile.local
