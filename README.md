# helm-github-releases-proxy

Read-only Helm repository proxy for charts published as GitHub release assets,
through chart-releaser, or from a local directory. Either GitHub mode can also
include one local chart directory. The service builds one aggregate index and
streams chart packages through the proxy.

## Quick start

Docker is required to run the proxy; Helm is required for the client commands
below.

```sh
docker run --rm -p 8080:8080 \
  -e GITHUB_OWNER=acme \
  -e GITHUB_REPO=charts \
  ljcanales/helm-github-releases-proxy:latest
```

Then add the repository to Helm:

```sh
helm repo add helm-github-releases-proxy http://localhost:8080
helm repo update
helm search repo helm-github-releases-proxy
```

In `github-releases` mode, release assets must match
`<chart-name>-<version>.tgz`; an optional `v` before the semantic version is
accepted. Other assets are skipped.

### Add local charts to a GitHub mode

Mount your chart directory read-only and set `LOCAL_PATH` to its absolute
container path to add local packages to either GitHub mode. Leave it unset or
empty to use GitHub alone. The example uses `github-releases`; set
`MODE=chart-releaser` to use a chart-releaser index.

```sh
docker run --rm -p 8080:8080 \
  -e MODE=github-releases \
  -e GITHUB_OWNER=acme \
  -e GITHUB_REPO=charts \
  -e LOCAL_PATH=/charts \
  --mount type=bind,src=/srv/helm-charts,dst=/charts,readonly \
  ljcanales/helm-github-releases-proxy:latest
```

If GitHub and local sources contain the same chart name and version, the
service retains GitHub metadata and lists GitHub download URLs first, followed
by the local download URL.

### Local-only charts

To serve charts from a directory without configuring or contacting GitHub,
mount the chart directory read-only and provide its absolute container path
as `LOCAL_PATH`. The host directory must exist and may be empty; an unset or
relative path is a configuration error. Chart archives must be direct children
of that directory.

```sh
docker run --rm -p 8080:8080 \
  -e MODE=local-only \
  -e LOCAL_PATH=/charts \
  --mount type=bind,src=/srv/helm-charts,dst=/charts,readonly \
  ljcanales/helm-github-releases-proxy:latest
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `MODE` | `github-releases` | `github-releases`, `chart-releaser`, or `local-only`. Other values are configuration errors. |
| `LOCAL_PATH` | Empty | Optional absolute local chart directory in either GitHub mode; required in `local-only`. The directory may be empty. |
| `GITHUB_OWNER` | Required for GitHub modes | GitHub repository owner. Ignored in `local-only` mode. |
| `GITHUB_REPO` | Required for GitHub modes | GitHub repository name. Ignored in `local-only` mode. |
| `GITHUB_TOKEN` | Empty | Optional token value for GitHub API requests. Ignored in `local-only` mode. |
| `CHART_RELEASER_PAGES_BRANCH` | `gh-pages` | Branch containing the chart-releaser `index.yaml` and any relative packages. Ignored outside `chart-releaser` mode. |
| `PORT` | `8080` | HTTP port, from `1` through `65535`. |
| `CACHE_TTL_SECONDS` | `60` | Fresh-index cache lifetime. `0` rebuilds on every index request. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN` (`WARNING` also accepted), or `ERROR`. |

For private repositories, supply a token with read access. In chart-releaser
mode, when configured, `GITHUB_TOKEN` authenticates branch index and relative
package reads. Direct requests to `github.com/.../releases/download/...` also
send the configured token but can still fail for private repositories because
of GitHub's private release-download authentication limitations. For private
chart-releaser repositories, keep packages on the configured branch and use
relative URLs in `index.yaml`. The former `HELM_REPOSITORIES` JSON configuration
is no longer supported.

A flat environment example is also available in `.env.example`. Edit its
values for your repository, then pass it to Docker:

```sh
docker run --rm -p 8080:8080 \
  --env-file .env.example \
  ljcanales/helm-github-releases-proxy:latest
```

To serve a chart-releaser index from its default `gh-pages` branch:

```sh
docker run --rm -p 8080:8080 \
  -e MODE=chart-releaser \
  -e GITHUB_OWNER=acme \
  -e GITHUB_REPO=charts \
  ljcanales/helm-github-releases-proxy:latest
```

Add `-e CHART_RELEASER_PAGES_BRANCH=release-pages` to use another branch.
When switching between the two GitHub modes, run `helm repo update` so clients
replace package URLs that use the old mode prefix.

## HTTP API

All routes use `GET`.

| Route | Purpose |
| --- | --- |
| `/healthz` | Liveness: `200` with `{"status":"ok"}` while serving. |
| `/readyz` | Configuration readiness: `200` when valid, otherwise `503` with `{"status":"not_ready"}`. |
| `/status` | Aggregate index and source status, cache timestamps, counts, and last error. |
| `/index.yaml` | Aggregate Helm index. |
| `/charts/github-releases/{asset}/{filename}` | Stream an indexed release asset by ID and filename. |
| `/charts/chart-releaser/{tag}/{filename}` | Stream a package from a chart-releaser release URL. |
| `/charts/chart-releaser/package-in-branch/{path}` | Stream a package stored with the branch index; nested safe paths are supported. |
| `/charts/local/{filename}` | Stream a package from the enabled local chart directory. |

Packages use `application/gzip` and an attachment filename. Missing assets,
invalid filenames, and inactive source routes return `404`; upstream download
failures return `502`.

## Operational behavior

Invalid configuration is logged at startup and reported by `/readyz`.
Startup index warming is asynchronous; readiness depends on configuration,
not GitHub availability.

The aggregate index status starts as `unknown`, becomes `ok` after a successful
build, and becomes `partial` if the source fails before any successful build.
After a successful build, a failed refresh keeps serving the last successful
index and reports `stale`. Failed sources include an error and indexed/skipped
counts in `/status`. Zero TTL disables the fresh-index cache but retains this
failure fallback.

## Docker Compose

The image runs as a non-root user and listens on port `8080` by default.
Save this example as `compose.yaml`, or merge the `helm-proxy` service into an
existing Compose file:

```yaml
services:
  helm-proxy:
    image: ljcanales/helm-github-releases-proxy:latest
    ports:
      - "8080:8080"
    environment:
      MODE: ${MODE:-github-releases}
      GITHUB_OWNER: ${GITHUB_OWNER:?Set GITHUB_OWNER}
      GITHUB_REPO: ${GITHUB_REPO:?Set GITHUB_REPO}
      GITHUB_TOKEN: ${GITHUB_TOKEN:-}
      CHART_RELEASER_PAGES_BRANCH: ${CHART_RELEASER_PAGES_BRANCH:-gh-pages}
      LOCAL_PATH: ${LOCAL_PATH:-}
      PORT: "8080"
      CACHE_TTL_SECONDS: "60"
      LOG_LEVEL: INFO
```

After saving the Compose file, run this from the directory containing it:

```sh
GITHUB_OWNER=acme GITHUB_REPO=charts docker compose up
```

To aggregate local charts in either GitHub mode, set `LOCAL_PATH` to the
container path `/charts` and mount the host directory `./charts` read-only.
Merge these settings under the existing `services.helm-proxy` service, keeping
its other environment settings:

```yaml
services:
  helm-proxy:
    environment:
      LOCAL_PATH: /charts
    volumes:
      - ./charts:/charts:ro
```

For local-only charts, mount the chart directory read-only and supply its
container path. An empty mounted directory is valid.

```yaml
services:
  helm-proxy:
    image: ljcanales/helm-github-releases-proxy:latest
    ports:
      - "8080:8080"
    environment:
      MODE: local-only
      LOCAL_PATH: /charts
      PORT: "8080"
      CACHE_TTL_SECONDS: "60"
      LOG_LEVEL: INFO
    volumes:
      - ./charts:/charts:ro
```
