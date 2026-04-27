from datetime import datetime

from pydantic import BaseModel, HttpUrl


class GitHubReleaseAsset(BaseModel):
    id: int
    name: str
    label: str | None = None
    content_type: str
    size: int
    digest: str | None = None
    browser_download_url: HttpUrl | None = None
    api_url: HttpUrl


class GitHubRelease(BaseModel):
    id: int
    tag_name: str
    name: str | None
    draft: bool
    prerelease: bool
    published_at: datetime | None
    assets: list[GitHubReleaseAsset]
