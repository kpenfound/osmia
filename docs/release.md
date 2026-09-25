# Releases

A release is a set of archives of the `osmia` binary for linux and darwin on
amd64 and arm64, and a `checksums.txt` of their SHA-256 sums. Windows is not
supported.

## Version

The release version and commit are stamped into the binary at link time, into
`github.com/kpenfound/osmia/internal/buildinfo.Version` and `.Commit`.
`osmia --version` prints them and exits 0 without contacting the service:

```console
$ osmia --version
osmia v0.1.0 (5e8a15b3915095d9e00bbc3ab3e5cc97970f2be1)
```

`osmia serve` reports the same values as `build.version` and `build.commit`
in [`/v1/health`](service.md). A binary built with a plain `go build` is
unstamped: it reports version `dev` and an empty commit, and `--version`
prints `osmia dev`.

## Building a release

The `release` Dagger module builds every archive from the workspace:

```sh
dagger api call release build --version v0.1.0 export --path dist
```

`--commit` sets the stamped commit; it defaults to the workspace's checked-out
`HEAD`, so build from a clean checkout of the tagged commit. The version must
not be empty or contain slashes or spaces.

`dist/` then holds:

- `osmia_<version>_<os>_<arch>.tar.gz` for `linux_amd64`, `linux_arm64`,
  `darwin_amd64` and `darwin_arm64`. Each unpacks to a directory of the same
  name holding the `osmia` binary, `LICENSE` and `README.md`.
- `checksums.txt`, in `sha256sum` format.

Attach these files to a GitHub release. The binaries are statically linked
(`CGO_ENABLED=0`) and the archives carry fixed timestamps and ownership.

`dagger check` includes `release:version-round-trip`, which builds a stamped
binary and checks that `osmia --version` prints the stamped version and
commit. The release build itself is not a check.

## Installing from an archive

Download the archive for your platform and `checksums.txt` from the release,
then verify, unpack and install:

```sh
version=v0.1.0
name=osmia_${version}_darwin_arm64   # or linux_amd64, linux_arm64, darwin_amd64
grep " $name.tar.gz\$" checksums.txt | shasum -a 256 -c   # sha256sum -c on linux
tar -xzf "$name.tar.gz"
sudo install -m 0755 "$name/osmia" /usr/local/bin/osmia
osmia --version
```

The binaries are not signed. On macOS, a binary downloaded with a browser
carries the quarantine attribute; clear it with
`xattr -d com.apple.quarantine /usr/local/bin/osmia` before running it.
