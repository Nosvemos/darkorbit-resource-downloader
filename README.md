# DarkOrbit Resource Downloader

[![CI](https://github.com/Nosvemos/Darkorbit-Resource-Downloader/actions/workflows/ci.yml/badge.svg)](https://github.com/Nosvemos/Darkorbit-Resource-Downloader/actions/workflows/ci.yml)
[![Release](https://github.com/Nosvemos/Darkorbit-Resource-Downloader/actions/workflows/release.yml/badge.svg)](https://github.com/Nosvemos/Darkorbit-Resource-Downloader/actions/workflows/release.yml)

A command-line tool that mirrors the static resource files of DarkOrbit (`darkorbit.com`, or a compatible server via `--base-url`) into a local directory. It reads the game's `filecollection` XML manifests, downloads the files they list in parallel, verifies each download against the manifest hash, and keeps a small state file so later runs only fetch what changed.

> **Disclaimer:** This is an unofficial project and is not affiliated with, endorsed by, or connected to Bigpoint. All downloaded game assets remain the property of their respective owners.

## Installation

### Prebuilt binaries

Download an archive for your platform from the [GitHub Releases](https://github.com/Nosvemos/Darkorbit-Resource-Downloader/releases) page:

- `darkorbit-downloader_<version>_windows_<arch>.zip`
- `darkorbit-downloader_<version>_linux_<arch>.tar.gz`
- `darkorbit-downloader_<version>_darwin_<arch>.tar.gz`

`<arch>` is `amd64` or `arm64`. Each release also contains `checksums.txt` with the SHA-256 sum of every archive, e.g. `sha256sum -c checksums.txt --ignore-missing`.

### With `go install`

Requires Go 1.26 or later:

```bash
go install github.com/Nosvemos/Darkorbit-Resource-Downloader/cmd/darkorbit-downloader@latest
```

### Build from source

```bash
git clone https://github.com/Nosvemos/Darkorbit-Resource-Downloader.git
cd Darkorbit-Resource-Downloader
make build                 # writes bin/darkorbit-downloader (.exe on Windows)
```

Without `make`:

```bash
go build -trimpath -o bin/darkorbit-downloader ./cmd/darkorbit-downloader
```

The version string is injected with
`-ldflags "-X github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/app.Version=v1.2.3"`
(`make build` does this from `git describe`). Without it, `version` falls back to the module version or VCS revision recorded by the Go toolchain.

## Usage

```
darkorbit-downloader <command> [flags]
```

Running the program without a command prints the usage text and exits with code 2. On Windows, when started by double-clicking, it waits for Enter so the text can be read.

### Commands

| Command | Description |
| :--- | :--- |
| `sync` | Fetches the manifests and core files, then downloads every selected resource that is missing, changed, or fails its hash check. |
| `plan` | Fetches the manifests (not the core `.swf` files or assets) and prints what `sync` would download and why. Writes no assets. |
| `fetch-manifests` | Fetches the manifests, language files and templates only. |
| `verify` | Checks the local mirror against the local manifests: reports missing files and fully re-hashes every file that has a manifest hash. Works offline, except that `--languages=all` contacts the server to discover the language list. |
| `version`, `--version` | Prints one line: `darkorbit-downloader <version> (<os>/<arch>, <go version>)`. |
| `help`, `-h`, `--help` | Prints the usage text. `<command> -h` does the same. |

The command must be the first argument; flags follow it.

### Flags

All commands accept the same flags.

| Flag | Default | Description |
| :--- | :--- | :--- |
| `--base-url` | `https://www.darkorbit.com` | Server to mirror. Must be an `http` or `https` URL with a host. |
| `--output` | `darkorbit-files` | Mirror directory, relative to the current directory unless absolute. |
| `--category` | `all` | Comma-separated categories: `all`, `spacemap`, `do_img`, `core`, `templates`, `unityApi`, `flashAPI`, `resources`. Unknown names are rejected. |
| `--languages` | `en` | Comma-separated language codes (e.g. `en,de,tr`), or `all` to use every language linked from the server's start page (falling back to the languages already present locally if that fails). |
| `--concurrency` | `8` | Maximum parallel downloads. Must be at least 1. |
| `--min-concurrency` | `1` | Lowest concurrency the auto-tuner may reduce to. Must be between 1 and `--concurrency`. |
| `--auto-tune-concurrency` | `true` | Lower concurrency after HTTP 429/503 responses and raise it again after a run of successes. |
| `--request-interval` | `250ms` | Minimum delay between starting two requests (Go duration, e.g. `0s`, `500ms`, `1s`). Must not be negative. |
| `--force` | `false` | Plan every selected resource for download, ignoring local files and state. |
| `--log-file` | `<output>/app.log` | Plain-text copy of the output (colour codes removed), appended to on every run. `--log-file ""` disables it. |
| `--no-color` | `false` | Disable coloured output. Setting the `NO_COLOR` environment variable has the same effect. |

### Examples

```bash
# Full sync of the default English resources
darkorbit-downloader sync

# A gentler sync against another server
darkorbit-downloader sync --base-url https://example.com --request-interval 500ms --concurrency 4

# Several languages
darkorbit-downloader sync --languages en,de,tr

# See what would be downloaded for the spacemap category
darkorbit-downloader plan --category spacemap

# Check an existing mirror
darkorbit-downloader verify --output darkorbit-files
```

### Files

Everything is written below `--output` (default `./darkorbit-files`):

| Path | Contents |
| :--- | :--- |
| `<output>/...` | The mirrored files, using the server's directory layout. |
| `<output>/.app-state.json` | State: for every verified resource, its manifest hash, URL, size and modification time. Written atomically. If it is corrupt, a warning is printed and it is rebuilt. |
| `<output>/app.log` | Log file (see `--log-file`). |
| `<output>/**/*.part` | Temporary download files. Removed at the start of every `sync`. |

Manifest entries whose path is empty, absolute, contains `:` or a backslash, or would escape the output directory (`../`) are skipped with a warning and never touched on disk. Resource paths that differ only in letter case are treated as the same file.

### How files are checked

DarkOrbit manifests publish an MD5 digest per file with its last byte replaced by `00`. A file matches when its MD5, with the last byte replaced by `00` in the same way, equals the manifest hash (case-insensitive). If a manifest hash does not end in `00`, the full MD5 is compared instead.

- **Downloads** are hashed while they are written to a `.part` file and are moved into place only if they match; a mismatch is retried once and then reported as a hash-mismatch failure.
- **`sync` and `plan`** skip re-hashing a local file when its size and modification time match the state file and its manifest hash has not changed. Otherwise the file is hashed. `--force` downloads everything.
- **`verify`** always re-hashes every file.

Failed requests are retried with backoff (up to 6 attempts) for network errors, stalled transfers (no data for 30 seconds), HTTP 408, 429 and 5xx. A `Retry-After` header is honoured.

Missing required files (the core `.swf` files, `crossdomain.xml` and the `spacemap/xml` manifests) abort a sync. Optional manifests and per-language files that the server does not have are skipped with a message.

### Exit codes

| Code | Meaning |
| :--- | :--- |
| `0` | Success. |
| `1` | Runtime error (network, file system, missing required file). |
| `2` | Usage error (no or unknown command, invalid flag). |
| `3` | `verify` found missing, mismatched or unreadable files or unparsable manifests, or some `sync` downloads failed. |
| `130` | Interrupted (Ctrl+C). |

Pressing Ctrl+C once stops a sync cleanly: in-flight downloads are abandoned, finished ones are recorded in the state file, and the program exits with code 130. Pressing it a second time terminates immediately.

## Development

```bash
make test    # go test -race -count=1 ./...
make lint    # golangci-lint run ./... (configuration in .golangci.yml)
make cover   # tests with a coverage summary
make tidy    # go mod tidy
```

Layout:

```
cmd/darkorbit-downloader/   entry point (signals, console setup)
internal/app/               command line, sync/plan/verify logic, output
internal/discovery/         seed manifests and language discovery
internal/downloader/        HTTP client, pacing, retries, concurrency tuning
internal/integrity/         manifest hash comparison
internal/manifest/          filecollection XML parser
internal/model/             shared types
internal/safepath/          safe resolution of server-supplied paths
internal/state/             .app-state.json
```

### CI and releases

- `.github/workflows/ci.yml` runs on pushes to `main` and on pull requests: `go mod tidy -diff`, `gofmt`, `go vet` and `go test -race` on Linux, Windows and macOS, plus `golangci-lint`.
- `.github/workflows/release.yml` runs when a `vX.Y.Z` tag is pushed, or manually from the Actions tab with a version input. It tests, builds Linux/macOS/Windows binaries for amd64 and arm64 with the version injected, and publishes them with `checksums.txt` as a GitHub release.

## License

Not yet specified.
