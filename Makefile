.PHONY: build install reasonix clean

BINARY := reasonix-telegram

build:
	go build -o $(BINARY) .

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)
	systemctl daemon-reload
	systemctl enable --now reasonix-telegram
	@echo "tail logs: journalctl -u reasonix-telegram -f"

reasonix:
	@if [ ! -d /tmp/reasonix-src ]; then \
		git clone --branch main-v2 https://github.com/esengine/DeepSeek-Reasonix /tmp/reasonix-src; \
	fi
	(cd /tmp/reasonix-src && go build -o /usr/local/bin/reasonix ./cmd/reasonix)
	@reasonix --version

clean:
	rm -f $(BINARY)
# Local overrides (e.g. pointing 'reasonix' at a fork). File is gitignored.
-include Makefile.local
