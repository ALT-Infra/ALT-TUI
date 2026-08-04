VERSION ?= dev
DIST := dist
LDFLAGS := -s -w -X altv1/internal/buildinfo.Version=$(VERSION)
NATIVE_MANIFEST := native/gui/Cargo.toml
NATIVE_LIBRARY := native/gui/target/release/libalt_native_gui.a
CARGO_HOME_PATH := $(shell sh -c 'printf "%s" "$${CARGO_HOME:-$${HOME}/.cargo}"')
RUST_REMAP_FLAGS := --remap-path-prefix=$(CURDIR)=/src/ALT-TUI --remap-path-prefix=$(CARGO_HOME_PATH)=/cargo

.PHONY: test test-go test-native native linux licenses embedded-bwrap clean

test: test-go test-native

test-go:
	go test ./...

test-native:
	RUSTFLAGS="$${RUSTFLAGS:+$${RUSTFLAGS} }$(RUST_REMAP_FLAGS)" \
		cargo test --manifest-path $(NATIVE_MANIFEST) --release

race:
	CGO_ENABLED=1 go test -race ./...

native:
	RUSTFLAGS="$${RUSTFLAGS:+$${RUSTFLAGS} }$(RUST_REMAP_FLAGS)" \
		cargo build --manifest-path $(NATIVE_MANIFEST) --release
	test -f $(NATIVE_LIBRARY)

licenses:
	test -x "$$(go env GOPATH)/bin/go-licenses"
	test -x "$$(command -v cargo-about)"
	set -eu; tmpdir=$$(mktemp -d); trap 'rm -rf "$$tmpdir"' EXIT; \
	GOFLAGS="-tags=nativegui" "$$(go env GOPATH)/bin/go-licenses" report \
		./cmd/alt \
		--ignore altv1 \
		--ignore github.com/cloudwego/eino-ext/components/model/openai \
		--ignore github.com/cloudwego/eino-ext/libs/acl/openai \
		--template licenses/go-notices.tmpl > "$$tmpdir/go.txt"; \
	cargo about generate \
		--manifest-path $(NATIVE_MANIFEST) \
		--config licenses/about.toml \
		--locked --fail \
		licenses/rust-notices.hbs \
		-o "$$tmpdir/rust.txt"; \
	test -s "$$tmpdir/go.txt"; \
	test -s "$$tmpdir/rust.txt"; \
	grep -q '^RUST DEPENDENCIES$$' "$$tmpdir/rust.txt"; \
	grep -q '^egui ' "$$tmpdir/rust.txt"; \
	grep -q '^eframe ' "$$tmpdir/rust.txt"; \
	{ \
		printf '%s\n\n' 'THIRD-PARTY SOFTWARE NOTICES' \
			'Generated from the ALT Linux production dependency graph.' \
			'Research clones and build/test-only tools are not redistributed and are excluded.'; \
		cat "$$tmpdir/go.txt"; \
		printf '%s\n' \
			'GO MODULE: github.com/cloudwego/eino-ext/components/model/openai v0.1.13' \
			'License classification: Apache-2.0' \
			'Source: https://github.com/cloudwego/eino-ext' \
			'' \
			'GO MODULE: github.com/cloudwego/eino-ext/libs/acl/openai v0.1.17' \
			'License classification: Apache-2.0' \
			'Source: https://github.com/cloudwego/eino-ext' \
			''; \
		cat references/tools/eino-ext-upstream/LICENSE; \
		printf '\n\n'; \
		cat "$$tmpdir/rust.txt"; \
		printf '%s\n' \
			'EMBEDDED RUNTIME ASSET' \
			'======================' \
			'' \
			'Bubblewrap 0.12.0' \
			'Source commit: 2f55bae38468d0c50cf5df87b1e481e882b63acb' \
			'License: LGPL-2.0-or-later' \
			'Source: https://github.com/containers/bubblewrap' \
			'SHA-256: 5724ad6485dc04210a5c8c8b74e20862eece00fab510a0ca91ea44a11e6ed167' \
			''; \
		cat references/bubblewrap/COPYING; \
	} > THIRD_PARTY_NOTICES.md; \
	cp THIRD_PARTY_NOTICES.md internal/licenses/THIRD_PARTY_NOTICES.txt; \
	cmp THIRD_PARTY_NOTICES.md internal/licenses/THIRD_PARTY_NOTICES.txt

# Rebuild the single-binary Linux sandbox fallback from the pinned source clone
# recorded in the generated notices. The checked-in asset lets ordinary ALT
# builds remain one command; this target is the reproducibility path.
embedded-bwrap:
	CFLAGS="-O2 -march=x86-64 -mtune=generic" \
		meson setup --wipe references/bubblewrap/build references/bubblewrap \
		--buildtype=release -Dselinux=disabled -Dtests=false -Dman=disabled \
		-Dbash_completion=disabled -Dzsh_completion=disabled
	ninja -C references/bubblewrap/build bwrap
	strip references/bubblewrap/build/bwrap
	install -Dm755 references/bubblewrap/build/bwrap internal/cli/assets/bwrap-linux-amd64
	sha256sum internal/cli/assets/bwrap-linux-amd64

linux: native
	mkdir -p $(DIST)
	NATIVE_SHA=$$(sha256sum $(NATIVE_LIBRARY) | cut -d' ' -f1); \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
		-tags nativegui \
		-trimpath \
		-ldflags "$(LDFLAGS) -X altv1/internal/nativegui.NativeBuildID=$$NATIVE_SHA" \
		-o $(DIST)/alt-linux-amd64 \
		./cmd/alt

clean:
	rm -f $(DIST)/alt-linux-amd64
