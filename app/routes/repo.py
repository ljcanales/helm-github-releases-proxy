from fastapi import APIRouter, HTTPException, Response
from fastapi.responses import StreamingResponse

from app.config import get_settings
from app.services.cache import get_cache
from app.services.chart_releaser_index import (
    ChartReleaserIndexError,
    ChartReleaserIndexRewriter,
)
from app.services.github_client import (
    GitHubClientAuthError,
    GitHubClientConfigError,
    GitHubClientNotFoundError,
    GitHubClientUpstreamError,
    GitHubReleasesClient,
)
from app.services.index_builder import IndexBuildError, IndexBuilder

router = APIRouter()


@router.get("/index.yaml", tags=["repo"])
async def get_index() -> Response:
    settings = get_settings()
    cache = get_cache()
    cache_key = "helm-index-yaml"

    cached_index = cache.get(cache_key)
    if cached_index is not None:
        return Response(content=cached_index, media_type="application/x-yaml")

    github_client = GitHubReleasesClient(settings)

    try:
        if settings.mode == "chart-releaser-action-support":
            upstream_index_yaml = await github_client.fetch_chart_releaser_index()
            index_yaml = ChartReleaserIndexRewriter(settings).rewrite(upstream_index_yaml)
        else:
            releases = await github_client.list_releases()
            index_yaml = await IndexBuilder(settings).build(releases)
    except GitHubClientConfigError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    except GitHubClientAuthError as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    except GitHubClientNotFoundError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except (GitHubClientUpstreamError, IndexBuildError, ChartReleaserIndexError) as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc

    cache.set(cache_key, index_yaml, settings.cache_ttl_seconds)
    return Response(content=index_yaml, media_type="application/x-yaml")


@router.get("/charts/{asset_id_or_tag}/{filename}", tags=["repo"])
async def get_chart(asset_id_or_tag: str, filename: str) -> Response:
    settings = get_settings()
    github_client = GitHubReleasesClient(settings)

    try:
        if settings.mode == "chart-releaser-action-support":
            stream = await github_client.stream_release_download(asset_id_or_tag, filename)
        else:
            stream = await github_client.stream_asset_by_id(int(asset_id_or_tag))
    except ValueError as exc:
        raise HTTPException(status_code=404, detail="GitHub release asset not found") from exc
    except GitHubClientConfigError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    except GitHubClientAuthError as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc
    except GitHubClientNotFoundError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except GitHubClientUpstreamError as exc:
        raise HTTPException(status_code=502, detail=str(exc)) from exc

    headers = {"Content-Disposition": f'attachment; filename="{filename}"'}
    return StreamingResponse(stream, media_type="application/gzip", headers=headers)
