from functools import lru_cache
from typing import Literal

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


Mode = Literal["releases-proxy", "chart-releaser-action-support"]


class Settings(BaseSettings):
    mode: Mode = Field(default="releases-proxy", alias="MODE")
    app_port: int = Field(default=8080, alias="APP_PORT")
    app_base_url: str = Field(default="", alias="APP_BASE_URL")
    github_owner: str = Field(default="", alias="GITHUB_OWNER")
    github_repo: str = Field(default="", alias="GITHUB_REPO")
    github_token: str = Field(default="", alias="GITHUB_TOKEN")
    chart_releaser_pages_branch: str = Field(
        default="gh-pages", alias="CHART_RELEASER_PAGES_BRANCH"
    )
    cache_ttl_seconds: int = Field(default=60, alias="CACHE_TTL_SECONDS")
    log_level: str = Field(default="INFO", alias="LOG_LEVEL")

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        case_sensitive=False,
        extra="ignore",
    )

    @property
    def chart_base_url(self) -> str:
        if self.app_base_url.strip():
            return self.app_base_url.strip().rstrip("/")
        return f"http://localhost:{self.app_port}"


@lru_cache
def get_settings() -> Settings:
    return Settings()
