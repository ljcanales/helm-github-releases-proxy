from collections.abc import MutableMapping
from urllib.parse import quote, unquote, urlparse

import yaml

from app.config import Settings


class ChartReleaserIndexError(Exception):
    pass


class ChartReleaserIndexRewriter:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings

    def rewrite(self, index_yaml: str) -> str:
        document = yaml.safe_load(index_yaml)
        if not isinstance(document, dict):
            raise ChartReleaserIndexError("chart-releaser index.yaml was not a mapping")

        entries = document.get("entries")
        if not isinstance(entries, dict):
            raise ChartReleaserIndexError("chart-releaser index.yaml was missing entries")

        for chart_name, chart_entries in entries.items():
            if not isinstance(chart_entries, list):
                raise ChartReleaserIndexError(f"{chart_name} entries were not a list")

            for chart_entry in chart_entries:
                if not isinstance(chart_entry, dict):
                    raise ChartReleaserIndexError(f"{chart_name} entry was not a mapping")
                self._rewrite_entry(chart_name, chart_entry)

        return yaml.safe_dump(document, sort_keys=False)

    def _rewrite_entry(self, chart_name: str, chart_entry: MutableMapping[object, object]) -> None:
        urls = chart_entry.get("urls")
        if not isinstance(urls, list) or not urls:
            raise ChartReleaserIndexError(f"{chart_name} entry was missing urls")

        rewritten_urls: list[str] = []
        for url in urls:
            if not isinstance(url, str) or not url.strip():
                raise ChartReleaserIndexError(f"{chart_name} entry had an invalid url")
            rewritten_urls.append(self._rewrite_url(url.strip()))

        chart_entry["urls"] = rewritten_urls

    def _rewrite_url(self, url: str) -> str:
        tag, filename = self._parse_release_download_url(url)
        return f"charts/{quote(tag, safe='')}/{quote(filename, safe='')}"

    def _parse_release_download_url(self, url: str) -> tuple[str, str]:
        parsed = urlparse(url)
        if parsed.scheme != "https" or parsed.netloc != "github.com":
            raise ChartReleaserIndexError(
                f"unsupported chart URL '{url}'; expected a GitHub release download URL"
            )

        path_parts = [unquote(part) for part in parsed.path.split("/") if part]
        if len(path_parts) < 6:
            raise ChartReleaserIndexError(
                f"unsupported chart URL '{url}'; expected a GitHub release download URL"
            )

        owner, repo, releases, download = path_parts[:4]
        filename = path_parts[-1]
        tag_parts = path_parts[4:-1]
        if (
            owner != self._settings.github_owner
            or repo != self._settings.github_repo
            or releases != "releases"
            or download != "download"
            or not tag_parts
            or not filename
        ):
            raise ChartReleaserIndexError(
                f"unsupported chart URL '{url}'; expected this repository's release download URL"
            )

        return "/".join(tag_parts), filename
