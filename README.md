# helm-github-releases-proxy

Helm chart repository proxy backed by GitHub Releases.

This service lets a GitHub repository behave like a classic Helm chart
repository. Helm clients can use `helm repo add`, `helm repo update`, and
`helm pull`, while chart packages stay stored as GitHub release assets.

The proxy solves two common gaps:

- GitHub Releases are convenient for storing chart archives, but they do not
  expose a Helm-compatible repository API by themselves.
- Private repositories, release assets, and generated chart URLs often need a
  stable service URL instead of direct GitHub download URLs.

The app exposes `/index.yaml` and `/charts/...` routes, reads from one GitHub
repository, and streams chart downloads through the proxy.

## Modes

Choose one mode based on how your chart repository is published.

### `releases-proxy`

Use this mode when chart `.tgz` files are uploaded directly as GitHub release
assets and you want this app to generate `index.yaml`.

Required configuration:

```sh
MODE=releases-proxy
GITHUB_OWNER=<my-username>
GITHUB_REPO=<my-charts>
```

In this mode, `GET /index.yaml` lists GitHub releases, filters release assets
ending in `.tgz`, derives chart name and version from asset names, and generates
Helm repository YAML. Chart URLs are generated as:

```text
{APP_BASE_URL}/charts/{asset_id}/{filename}
```

When `APP_BASE_URL` is empty, URLs use `http://localhost:{APP_PORT}`.

Chart asset filenames must follow:

```text
<chart-name>-<version>.tgz
```

### `chart-releaser-action-support`

Use this mode when `helm/chart-releaser-action` already publishes an
`index.yaml` file to a GitHub Pages branch and you want this app to rewrite its
chart URLs through the proxy.

Required configuration:

```sh
MODE=chart-releaser-action-support
GITHUB_OWNER=<my-username>
GITHUB_REPO=<my-charts>
CHART_RELEASER_PAGES_BRANCH=gh-pages # if not provided, defaults to gh-pages
```

In this mode, `GET /index.yaml` fetches the root `index.yaml` from
`CHART_RELEASER_PAGES_BRANCH`, preserves chart metadata, and rewrites GitHub
release download URLs from:

```text
https://github.com/{owner}/{repo}/releases/download/{tag}/{filename}
```

to:

```text
{APP_BASE_URL}/charts/{tag}/{filename}
```

Relative package URLs, such as chart-releaser's `--packages-with-index` output,
are not supported.

## Configuration

- `MODE`: `releases-proxy` or `chart-releaser-action-support`; defaults to
  `releases-proxy`
- `APP_PORT`: port used by Uvicorn; defaults to `8080`
- `APP_BASE_URL`: public base URL used in generated chart download URLs; defaults to `http://localhost:{APP_PORT}`
- `GITHUB_OWNER`: GitHub repository owner or organization
- `GITHUB_REPO`: GitHub repository name
- `GITHUB_TOKEN`: optional for public repositories; required for private
  repositories
- `CHART_RELEASER_PAGES_BRANCH`: branch containing chart-releaser `index.yaml`;
  defaults to `gh-pages` (only apply when running in `chart-releaser-action-support` mode)
- `CACHE_TTL_SECONDS`: TTL for generated `index.yaml` responses; defaults to
  `60`
- `LOG_LEVEL`: Python logging level; defaults to `INFO`

Set `APP_BASE_URL` to the URL Helm clients use to reach this service. For
example, use `https://charts.example.com` behind a reverse proxy, or leave it
empty for local development where Helm reaches `http://localhost:8080`.

For public repositories, `GITHUB_TOKEN` can be left empty. A token is still
recommended for higher GitHub API rate limits. For private repositories, use a
fine-grained GitHub token with read-only repository permissions. This proxy only
reads releases, assets, and repository contents; it does not publish packages or
write to GitHub.

## Run From Source

Clone the repository, create and activate a virtual environment, then install dependencies:

```sh
pip install -r requirements.txt
```

Copy and edit the local environment file:

```sh
cp .env.example .env
```

Start the server:

```sh
uvicorn app.main:app --reload --host 0.0.0.0 --port 8080
```

Verify it is running:

```sh
curl http://localhost:8080/health
```

## Run With Docker

Pull and run the published image:

```sh
docker pull ljcanales/helm-github-releases-proxy
docker run --rm -p 8080:8080 \
  -e MODE=releases-proxy \
  -e APP_PORT=8080 \
  -e GITHUB_OWNER=username \
  -e GITHUB_REPO=my_charts_repo \
  -e GITHUB_TOKEN=github_readonly_token \
  ljcanales/helm-github-releases-proxy
```

To build the image locally instead:

```sh
docker build -t helm-github-releases-proxy .
docker run --rm -p 8080:8080 \
  -e MODE=releases-proxy \
  -e APP_PORT=8080 \
  -e APP_BASE_URL=http://localhost:8080 \
  -e GITHUB_OWNER=acme \
  -e GITHUB_REPO=charts \
  helm-github-releases-proxy
```

## Docker Compose

Example Compose service using the published image:

```yaml
services:
  helm-github-releases-proxy:
    image: ljcanales/helm-github-releases-proxy
    ports:
      - "8080:8080"
    environment:
      MODE: releases-proxy
      APP_PORT: "8080"
      APP_BASE_URL: http://localhost:8080
      GITHUB_OWNER: my-username
      GITHUB_REPO: my-charts-repository
      GITHUB_TOKEN: github_pat_readonly_token
      CACHE_TTL_SECONDS: "60"
      LOG_LEVEL: INFO
```

For `chart-releaser-action-support`, change the mode and include the Pages
branch:

```yaml
environment:
  MODE: chart-releaser-action-support
  CHART_RELEASER_PAGES_BRANCH: gh-pages
```

## Helm Usage

With the app running and reachable by Helm:

```sh
helm repo add helm-github-releases-proxy http://localhost:8080
helm repo update
helm search repo helm-github-releases-proxy
```

## Endpoints

- `GET /health` returns `200`
- `GET /index.yaml` returns Helm repository YAML
- `GET /charts/{asset_id_or_tag}/{filename}` streams the matching chart archive

Chart download behavior depends on `MODE`.

In `releases-proxy` mode, `{asset_id_or_tag}` is a GitHub release asset id. The
app streams from the GitHub release asset API.

In `chart-releaser-action-support` mode, `{asset_id_or_tag}` is a release tag.
The app streams from:

```text
https://github.com/{owner}/{repo}/releases/download/{tag}/{filename}
```

GitHub configuration errors return `500`. GitHub authentication and upstream
failures return `502`. Missing repositories or chart assets return `404`.

## Current Limitations

- single GitHub repository only
- no end-user authentication layer
- no metrics or tracing
- no chart upload API
