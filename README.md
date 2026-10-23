# Ptah Spanner provider

This repository will hold the executable Spanner provider for
[Ptah #4204](https://github.com/stokaro/ptah/issues/4204).

**It is a scaffold with no features yet.** The executable negotiates with a
Ptah host, declares the `spanner` engine, its identifier rules and its release
lines, and stops there. It implements no service. The session service the
runtime requires refuses every `Open` with `Unimplemented`. A host that
negotiates with it finds no schema inspection, planning, rendering or
execution to call, and no database address is read or contacted.

## What the host does today

The host's built-in `spanner` dialect reaches Cloud Spanner through its PostgreSQL interface, with PostgreSQL's SQL and PGAdapter as the driver; it is measured against the Spanner emulator behind PGAdapter and certified for no line. Its provider catalog also reserves the engine name `spanner`, the aliases `cloudspanner`, `google-spanner` and `google_spanner`, and this repository as the official provider. Recognition does not install or authorize an executable. See the host's [Spanner provider page](https://github.com/stokaro/ptah/blob/master/docs/site/src/content/docs/extend/providers/spanner.md).

This scaffold covers the GoogleSQL dialect only. How an engine name that the host already serves natively will select this provider is a host decision this repository does not make.

## What the description declares

- Provider ID: `github.com/stokaro/ptah-spanner-plugin`.
- Engine: `spanner`, with the aliases `cloudspanner`, `google-spanner` and `google_spanner`.
- Identifier rules: table, column and index names compare without regard to ASCII case, and an index name is unique in the database, not in its table.
- Release lines: the single line `0`, because the managed service has no release lines. The host measures the PostgreSQL interface through an emulator, which is not the GoogleSQL dialect, so that measurement is not reused here. Every line is best effort and states why it runs
  no job. The scaffold certifies none.

## Roadmap

First comes a connection through the Cloud Spanner client and the session service, then catalog inspection and DDL execution in GoogleSQL. Rendering and planning follow, and each service is advertised in the description only when it exists.

## Build and test

The module pins the published experimental SDK, `github.com/stokaro/ptah/provider-sdk`,
by pseudo-version. It needs neither the Ptah application source nor a local SDK
checkout and has no `replace` directive.

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-spanner ./cmd/ptah-plugin-spanner
```

The executable requires the SDK's authenticated local subprocess bootstrap. Run
it through a Ptah SDK client, not as a command-line tool. Print the release
lines it declares with:

```sh
GOWORK=off go run ./cmd/release-lines
```

## Releases

A tag that starts with `v` runs `.github/workflows/release.yml`, which
publishes a GitHub release through GoReleaser (`.goreleaser.yaml`). Each
release carries one archive per platform, a `checksums.txt` file, and
`release-lines.json`:

```text
ptah-plugin-spanner_<version>_<os>_<arch>.tar.gz   (.zip on Windows)
checksums.txt
release-lines.json
```

`<version>` is the tag without the leading `v`. The platforms are linux,
darwin and windows on amd64 and arm64. Every archive holds the
`ptah-plugin-spanner` binary (`ptah-plugin-spanner.exe` on Windows),
`README.md` and `LICENSE`. The binaries are built with `CGO_ENABLED=0` and
`-trimpath`.

`checksums.txt` lists the SHA-256 digest of every archive and of
`release-lines.json` in `sha256sum` format (`<hex digest>  <file name>`). A
host pins a release by version and by the digest listed there for its
platform's archive.

Pull requests that touch the release inputs run `goreleaser check` and a
snapshot build, then verify the snapshot's `checksums.txt`. To try it locally:

```sh
GOWORK=off goreleaser release --snapshot --clean
(cd dist && cp ../.release/release-lines.json . && shasum -a 256 -c checksums.txt)
```

## License

MIT. See `LICENSE`.
