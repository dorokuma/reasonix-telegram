.PHONY: build install start deploy push reasonix clean test vet

VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
BINARY := reasonix-telegram
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build
	rm -f /usr/local/bin/$(BINARY)
	cp $(BINARY) /usr/local/bin/$(BINARY)
	chmod 0755 /usr/local/bin/$(BINARY)
	systemctl daemon-reload
	@echo "tail logs: journalctl -u reasonix-telegram -f"

start:
	systemctl enable --now reasonix-telegram

deploy: test vet build install
	@echo "=== deploy complete: built, tested, installed ==="
	@echo ">>> restart manually if service is running: systemctl restart reasonix-telegram"

push:
	git add -A
	@echo "--- git status ---"
	git status --short
	@echo "---"
	@read -p "Commit message: " msg; \
		git commit -m "$$msg" && git push
	@echo "=== pushed ==="

reasonix:
	(cd ../reasonix && CGO_ENABLED=0 go build -o reasonix ./cmd/reasonix && rm -f /usr/local/bin/reasonix && cp reasonix /usr/local/bin/reasonix)
	@reasonix --version

test:
	go test ./... -v -count=1

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
# Local overrides (e.g. pointing 'reasonix' at a fork). File is gitignored.
-include Makefile.local
