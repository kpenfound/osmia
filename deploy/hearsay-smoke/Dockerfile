# The Hearsay image the smoke run uses: the statically linked binary built from
# the Hearsay tree under test, on the same Alpine base and entrypoint as the
# image Hearsay publishes (its Dagger module's `image`). The binary is built
# by scripts/hearsay-compose-smoke.sh.
FROM alpine:3.22
COPY hearsay /usr/local/bin/hearsay
USER nobody
ENTRYPOINT ["/usr/local/bin/hearsay"]
