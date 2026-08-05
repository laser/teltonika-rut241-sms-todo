set shell := ["bash", "-euc"]

binary := "teltonika-rut241-sms-todo"
build_dir := "dist"
version := env_var_or_default("VERSION", `git describe --always --dirty`)
package_name := binary + "-" + version + "-linux-armv7"
archive := build_dir + "/" + package_name + ".tar.gz"

default:
    @just --list

# Run tests and static analysis.
check:
    go test ./...
    go vet ./...

# Build all supported binaries.
build: build-macos build-pi

# Build native binaries for Apple Silicon and Intel Macs.
build-macos: build-macos-arm64 build-macos-amd64

# Build the Apple Silicon macOS binary.
build-macos-arm64:
    mkdir -p "{{build_dir}}"
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "{{build_dir}}/{{binary}}-darwin-arm64" .

# Build the Intel macOS binary.
build-macos-amd64:
    mkdir -p "{{build_dir}}"
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "{{build_dir}}/{{binary}}-darwin-amd64" .

# Build the static ARMv7 binary for Raspberry Pi 3.
build-pi:
    mkdir -p "{{build_dir}}"
    CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o "{{build_dir}}/{{binary}}-linux-armv7" .

# Package the Raspberry Pi binary, examples, service unit, and documentation.
package: build-pi
    #!/usr/bin/env bash
    set -euo pipefail
    staging_dir="$(mktemp -d "${TMPDIR:-/tmp}/{{package_name}}.XXXXXX")"
    trap 'rm -rf "$staging_dir"' EXIT
    package_dir="$staging_dir/{{package_name}}"
    mkdir -p "$package_dir/systemd"
    install -m 755 "{{build_dir}}/{{binary}}-linux-armv7" "$package_dir/{{binary}}"
    install -m 644 README.md config.example.json "$package_dir/"
    install -m 644 systemd/{{binary}}.service systemd/{{binary}}.env.example "$package_dir/systemd/"
    COPYFILE_DISABLE=1 tar --no-xattrs -C "$staging_dir" -czf "{{archive}}" "{{package_name}}"
    shasum -a 256 "{{archive}}" > "{{archive}}.sha256"
    echo "Created {{archive}}"

# Publish the Raspberry Pi archive and checksum as a GitHub Release.
release: package
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${VERSION:-}" ]]; then
        echo "Set a release tag, for example: VERSION=v0.1.0 just release" >&2
        exit 2
    fi
    if ! git diff --quiet || ! git diff --cached --quiet; then
        echo "Refusing to release from a dirty working tree." >&2
        exit 2
    fi
    git fetch origin main
    if [[ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]]; then
        echo "Refusing to release: HEAD does not match origin/main." >&2
        exit 2
    fi
    gh release create "$VERSION" \
        "{{archive}}#Raspberry Pi ARMv7 package" \
        "{{archive}}.sha256#SHA-256 checksum" \
        --generate-notes \
        --title "$VERSION" \
        --target main
