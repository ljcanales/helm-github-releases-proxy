# helm-github-releases-proxy

**helm-github-releases-proxy** is a read-only Helm repository proxy written in Go.
It serves charts from GitHub releases, chart-releaser repositories, or a local
directory through a single Helm repository.

The proxy builds the repository index and streams chart downloads. Either GitHub
mode can also include local chart packages. Run it with Docker using the image
published on [Docker Hub](https://hub.docker.com/r/ljcanales/helm-github-releases-proxy).

- [Endpoints](#endpoints)
- [Modes](#modes)
- [Configuration](#configuration)
- [Docker Compose](#docker-compose)

## Endpoints

All endpoints use `GET`.

| Endpoint | Purpose |
| --- | --- |
| `/index.yaml` | Helm repository index used by `helm repo add` and `helm repo update`. |
| `/charts/...` | Download chart packages from the configured sources. |
| `/healthz` | Check that the server is running. |
| `/readyz` | Check that configuration is valid; does not check source availability. |
| `/status` | Inspect index status, source errors, chart counts, and cache timestamps. |

Chart download routes serve GitHub release assets, chart-releaser packages stored
in releases or on the configured branch, and local packages. Helm follows the
download URLs generated in the index automatically.

`/healthz` returns `200` with `{"status":"ok"}`. `/readyz` returns `200` when
configuration is valid, otherwise `503` with `{"status":"not_ready"}`. Invalid
configuration is also logged at startup. The initial index build runs
asynchronously; readiness does not wait for it to finish.

Chart downloads use `application/gzip` with an attachment filename. Missing
packages, invalid filenames, and inactive source routes return `404`; upstream
download failures return `502`.

## Modes

These examples use Docker. Replace `<repo-owner>` and `<repo-name>` with your
GitHub repository owner and name before running the commands. Helm is required
for the client commands at the end of this section.

### `github-releases` — default

Serve chart packages published as GitHub release assets. Asset filenames must
follow `<chart-name>-<version>.tgz`, where the version is a semantic version; an
optional `v` before the version is accepted. Other assets are skipped.

```sh
docker run --rm -p 8080:8080 \
  -e GITHUB_OWNER='<repo-owner>' \
  -e GITHUB_REPO='<repo-name>' \
  ljcanales/helm-github-releases-proxy:latest
```

### `chart-releaser`

Serve charts from a chart-releaser `index.yaml`, read from the repository's
`gh-pages` branch by default. Packages can be GitHub release assets or files
stored on that branch. Use `CHART_RELEASER_PAGES_BRANCH` to select another branch.

```sh
docker run --rm -p 8080:8080 \
  -e MODE=chart-releaser \
  -e GITHUB_OWNER='<repo-owner>' \
  -e GITHUB_REPO='<repo-name>' \
  ljcanales/helm-github-releases-proxy:latest
```

### `local-only`

Serve packaged charts from a local directory without configuring or contacting
GitHub. Replace `/srv/helm-charts` with an existing directory containing your
`.tgz` chart packages.

```sh
docker run --rm -p 8080:8080 \
  -e MODE=local-only \
  -e LOCAL_PATH=/charts \
  --mount type=bind,src=/srv/helm-charts,dst=/charts,readonly \
  ljcanales/helm-github-releases-proxy:latest
```

Packages must be directly inside the mounted directory; subdirectories and
symlinks are skipped. Filenames must match the chart name and version in the
package's `Chart.yaml`. An empty directory is valid.

### Include local charts with GitHub

Either GitHub mode can include local chart packages. The GitHub and local sources
are aggregated into a single Helm repository, so Helm can discover and download
charts from both.

Add these options before the image name in either GitHub mode's Docker command,
replacing `/srv/helm-charts` with your chart directory:

```sh
-e LOCAL_PATH=/charts \
--mount type=bind,src=/srv/helm-charts,dst=/charts,readonly \
```

If both sources contain the same chart name and version, the proxy retains GitHub
metadata and lists GitHub download URLs first, followed by the local URL.

### Use with Helm

After starting the proxy in any mode:

```sh
helm repo add helm-proxy http://localhost:8080
helm repo update
helm search repo helm-proxy
```

Run `helm repo update` after switching modes so Helm refreshes the chart download
URLs.

## Configuration

Configure the proxy with environment variables. See [.env.example](.env.example)
for a starting point; Docker can load it using `--env-file .env.example`.

| Variable | Default | Description |
| --- | --- | --- |
| `MODE` | `github-releases` | Source mode: `github-releases`, `chart-releaser`, or `local-only`. Values are case-sensitive; other values are configuration errors. |
| `GITHUB_OWNER` | Empty | GitHub repository owner. Required in either GitHub mode; ignored in `local-only`. |
| `GITHUB_REPO` | Empty | GitHub repository name. Required in either GitHub mode; ignored in `local-only`. |
| `GITHUB_TOKEN` | Empty | Optional token for GitHub API and download requests. Ignored in `local-only`. |
| `CHART_RELEASER_PAGES_BRANCH` | `gh-pages` | Branch containing the chart-releaser `index.yaml` and any relative packages. Used only in `chart-releaser` mode. |
| `LOCAL_PATH` | Empty | Absolute chart directory path inside the container. Optional in either GitHub mode; required in `local-only`. The directory may be empty. |
| `PORT` | `8080` | HTTP listening port. Must be an integer from `1` through `65535`; update Docker's port mapping if changed. |
| `CACHE_TTL_SECONDS` | `60` | Fresh-index cache lifetime in seconds. Must be a nonnegative integer; `0` rebuilds on every index request. |
| `LOG_LEVEL` | `INFO` | Logging level: `DEBUG`, `INFO`, `WARN` (`WARNING` also accepted), or `ERROR`. Values are case-insensitive. |

**Private repositories:** supply `GITHUB_TOKEN` with read access. In
`chart-releaser` mode, the token authenticates branch index and relative package
reads. Direct private release downloads can still fail due to GitHub's release
download authentication limitations. For private chart-releaser repositories,
store packages on the configured branch and use relative URLs in `index.yaml`.

**Cache and source status:** `/status` starts as `unknown`, becomes `ok` after a
successful index build, and reports `partial` when sources fail without a previous
successful index. After a successful build, a failed refresh keeps serving the
last successful index and reports `stale`. Setting the cache TTL to `0` retains
this fallback. Source errors and indexed/skipped counts are available in `/status`.
An inaccessible local directory is a source error, even when `/readyz` reports
valid configuration.

## Docker Compose

Save this example as `compose.yaml` and replace `<repo-owner>` and `<repo-name>`
with your GitHub repository owner and name:

```yaml
services:
  helm-proxy:
    image: ljcanales/helm-github-releases-proxy:latest
    ports:
      - "8080:8080"
    environment:
      MODE: github-releases
      GITHUB_OWNER: "<repo-owner>"
      GITHUB_REPO: "<repo-name>"
      GITHUB_TOKEN: "<token>"
      # Optional: aggregate local chart packages with GitHub charts.
      # LOCAL_PATH: /charts
    # volumes:
    #   - ./charts:/charts:ro
```

Start the proxy from the directory containing the Compose file:

```sh
docker compose up -d
```

To include local charts, create a readable `./charts` directory and uncomment
`LOCAL_PATH`, `volumes`, and the mount entry. Both sources will be aggregated into
one Helm repository.

To use chart-releaser, set `MODE: chart-releaser`; optionally add
`CHART_RELEASER_PAGES_BRANCH` under `environment` to select a different branch.
For local-only charts, set `MODE: local-only`, enable the local path and mount,
and remove the `GITHUB_OWNER`, `GITHUB_REPO`, and `GITHUB_TOKEN` entries.

Then use the [Helm commands above](#use-with-helm) to connect to the repository.
