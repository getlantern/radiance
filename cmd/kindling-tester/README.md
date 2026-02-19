# Kindling transport tester

Tests individual [kindling](../../kindling) transports (proxyless, fronted, amp, dnstt).
It receives all its arguments via environment variables and uses the kindling HTTP client directly

## Environment variables

### Required

- `DEVICE_ID`: The device ID to use.
- `USER_ID`: The user ID to use.
- `TOKEN`: The auth token to use.
- `RUN_ID`: The run ID. Added to traces as `pinger-id` — useful for looking up a specific run.
- `TARGET_URL`: The URL that will be fetched through kindling.
- `DATA`: Directory for config files, logs, and output artefacts (`output.txt`, `timing.txt`, `success`).

- `TRANSPORT`: The kindling transport to test. One of: `proxyless`, `fronted`, `amp`, `dnstt`.

## CLI usage

```bash
DEVICE_ID=1234 USER_ID=123 TOKEN=mytoken RUN_ID=run1 TARGET_URL=https://example.com DATA=./mydir \
    TRANSPORT=proxyless \
    ./kindling-tester
```

Replace `proxyless` with the transport you want to test (`fronted`, `amp`, `dnstt`).

Upon success the tester writes:
- `DATA/success` — empty marker file
- `DATA/output.txt` — response body
- `DATA/timing.txt` — timing breakdown (connect + fetch latency)

## Docker usage

A separate image is built per transport. Each image bakes `TRANSPORT` in at build time via `ENV TRANSPORT=${TRANSPORT}`.

### Building

```bash
docker build --build-arg TRANSPORT=proxyless -t radiance-kindling-tester:proxyless -f ./docker/Dockerfile.kindling-tester .
docker build --build-arg TRANSPORT=fronted   -t radiance-kindling-tester:fronted   -f ./docker/Dockerfile.kindling-tester .
docker build --build-arg TRANSPORT=amp       -t radiance-kindling-tester:amp       -f ./docker/Dockerfile.kindling-tester .
docker build --build-arg TRANSPORT=dnstt     -t radiance-kindling-tester:dnstt     -f ./docker/Dockerfile.kindling-tester .
```

### Running

```bash
docker run --rm -v ./mydir:/output \
    -e DEVICE_ID=1234 \
    -e USER_ID=1234 \
    -e TOKEN=mytoken \
    -e RUN_ID=run1 \
    -e TARGET_URL=https://example.com \
    -e DATA=/output \
    radiance-kindling-tester:proxyless
```

Swap the image tag to test a different transport; no other flags need to change.
