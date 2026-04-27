from collections.abc import AsyncIterator
from typing import Any
from urllib.parse import quote

import httpx

from app.config import Settings
from app.models.github import GitHubRelease, GitHubReleaseAsset

GITHUB_API_BASE_URL = "https://api.github.com"


class GitHubClientError(Exception):
    pass


class GitHubClientConfigError(GitHubClientError):
    pass


class GitHubClientAuthError(GitHubClientError):
    pass


class GitHubClientNotFoundError(GitHubClientError):
    pass


class GitHubClientUpstreamError(GitHubClientError):
    pass


class GitHubReleasesClient:
    def __init__(self, settings: Settings, http_client: httpx.AsyncClient | None = None) -> None:
        self._settings = settings
        self._http_client = http_client

    async def list_releases(self) -> list[GitHubRelease]:
        self._validate_settings()
        payload = await self._fetch_releases()
        return [self._normalize_release(release) for release in payload]

    async def fetch_chart_releaser_index(self) -> str:
        self._validate_settings()
        branch = self._settings.chart_releaser_pages_branch
        if not branch.strip():
            raise GitHubClientConfigError("CHART_RELEASER_PAGES_BRANCH is required")

        url = (
            f"{GITHUB_API_BASE_URL}/repos/"
            f"{self._settings.github_owner}/{self._settings.github_repo}/contents/index.yaml"
        )
        headers = self._build_headers("application/vnd.github.raw+json")

        try:
            if self._http_client is not None:
                response = await self._http_client.get(url, headers=headers, params={"ref": branch})
            else:
                async with httpx.AsyncClient(timeout=20.0) as client:
                    response = await client.get(url, headers=headers, params={"ref": branch})
        except httpx.RequestError as exc:
            raise GitHubClientUpstreamError("GitHub index request could not be completed") from exc

        self._raise_for_status(response)
        return response.text

    async def download_asset_bytes(self, asset_api_url: str) -> bytes:
        self._validate_settings()
        headers = self._build_headers("application/octet-stream")

        try:
            if self._http_client is not None:
                response = await self._http_client.get(asset_api_url, headers=headers)
            else:
                async with httpx.AsyncClient(timeout=60.0, follow_redirects=True) as client:
                    response = await client.get(asset_api_url, headers=headers)
        except httpx.RequestError as exc:
            raise GitHubClientUpstreamError("GitHub asset request could not be completed") from exc

        self._raise_for_status(response)
        return response.content

    async def stream_asset(self, asset_api_url: str, chunk_size: int = 65536) -> AsyncIterator[bytes]:
        self._validate_settings()
        headers = self._build_headers("application/octet-stream")

        async def iterator() -> AsyncIterator[bytes]:
            try:
                async with httpx.AsyncClient(timeout=60.0, follow_redirects=True) as client:
                    async with client.stream("GET", asset_api_url, headers=headers) as response:
                        self._raise_for_status(response)
                        async for chunk in response.aiter_bytes(chunk_size):
                            yield chunk
            except httpx.RequestError as exc:
                raise GitHubClientUpstreamError("GitHub asset request could not be completed") from exc

        return iterator()

    async def stream_asset_by_id(
        self, asset_id: int, chunk_size: int = 65536
    ) -> AsyncIterator[bytes]:
        return await self.stream_asset(self._build_asset_api_url(asset_id), chunk_size)

    async def stream_release_download(
        self, tag: str, filename: str, chunk_size: int = 65536
    ) -> AsyncIterator[bytes]:
        self._validate_settings()
        url = self._build_release_download_url(tag, filename)
        headers = self._build_headers("application/octet-stream")

        async def iterator() -> AsyncIterator[bytes]:
            try:
                async with httpx.AsyncClient(timeout=60.0, follow_redirects=True) as client:
                    async with client.stream("GET", url, headers=headers) as response:
                        self._raise_for_status(response)
                        async for chunk in response.aiter_bytes(chunk_size):
                            yield chunk
            except httpx.RequestError as exc:
                raise GitHubClientUpstreamError(
                    "GitHub release download request could not be completed"
                ) from exc

        return iterator()

    def _build_asset_api_url(self, asset_id: int) -> str:
        return (
            f"{GITHUB_API_BASE_URL}/repos/"
            f"{self._settings.github_owner}/{self._settings.github_repo}/releases/assets/{asset_id}"
        )

    def _build_release_download_url(self, tag: str, filename: str) -> str:
        return (
            "https://github.com/"
            f"{self._settings.github_owner}/{self._settings.github_repo}/releases/download/"
            f"{quote(tag, safe='')}/{quote(filename, safe='')}"
        )

    def _validate_settings(self) -> None:
        if not self._settings.github_owner:
            raise GitHubClientConfigError("GITHUB_OWNER is required")
        if not self._settings.github_repo:
            raise GitHubClientConfigError("GITHUB_REPO is required")

    async def _fetch_releases(self) -> list[dict[str, Any]]:
        url = (
            f"{GITHUB_API_BASE_URL}/repos/"
            f"{self._settings.github_owner}/{self._settings.github_repo}/releases"
        )
        headers = self._build_headers("application/vnd.github+json")

        try:
            if self._http_client is not None:
                response = await self._http_client.get(url, headers=headers)
            else:
                async with httpx.AsyncClient(timeout=20.0) as client:
                    response = await client.get(url, headers=headers)
        except httpx.RequestError as exc:
            raise GitHubClientUpstreamError("GitHub releases request could not be completed") from exc

        self._raise_for_status(response)

        data = response.json()
        if not isinstance(data, list):
            raise GitHubClientUpstreamError("GitHub releases response was not a list")
        return data

    def _build_headers(self, accept: str) -> dict[str, str]:
        headers = {
            "Accept": accept,
            "X-GitHub-Api-Version": "2022-11-28",
        }
        if self._settings.github_token:
            headers["Authorization"] = f"Bearer {self._settings.github_token}"
        return headers

    def _raise_for_status(self, response: httpx.Response) -> None:
        if response.status_code < 400:
            return
        if response.status_code in (401, 403):
            raise GitHubClientAuthError("GitHub authentication failed")
        if response.status_code == 404:
            raise GitHubClientNotFoundError("GitHub repository not found")

        message = self._extract_error_message(response)
        raise GitHubClientUpstreamError(
            f"GitHub releases request failed with status {response.status_code}: {message}"
        )

    def _extract_error_message(self, response: httpx.Response) -> str:
        try:
            payload = response.json()
        except ValueError:
            return response.text or "unknown error"

        if isinstance(payload, dict):
            message = payload.get("message")
            if isinstance(message, str) and message:
                return message
        return "unknown error"

    def _normalize_release(self, payload: dict[str, Any]) -> GitHubRelease:
        assets = payload.get("assets") or []
        return GitHubRelease(
            id=payload["id"],
            tag_name=payload["tag_name"],
            name=payload.get("name"),
            draft=payload.get("draft", False),
            prerelease=payload.get("prerelease", False),
            published_at=payload.get("published_at"),
            assets=[self._normalize_asset(asset) for asset in assets],
        )

    def _normalize_asset(self, payload: dict[str, Any]) -> GitHubReleaseAsset:
        return GitHubReleaseAsset(
            id=payload["id"],
            name=payload["name"],
            label=payload.get("label"),
            content_type=payload.get("content_type", "application/octet-stream"),
            size=payload.get("size", 0),
            digest=payload.get("digest"),
            browser_download_url=payload.get("browser_download_url"),
            api_url=payload["url"],
        )
