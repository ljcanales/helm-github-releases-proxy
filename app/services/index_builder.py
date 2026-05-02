from collections import defaultdict
from collections.abc import Mapping
from datetime import UTC, datetime
import re
from urllib.parse import quote

import yaml

from app.config import Settings
from app.models.github import GitHubRelease, GitHubReleaseAsset


class IndexBuildError(Exception):
    pass


CHART_FILENAME_PATTERN = re.compile(
    r"^(?P<name>.+)-(?P<version>v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$"
)


class IndexBuilder:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings

    async def build(self, releases: list[GitHubRelease]) -> str:
        entries: dict[str, list[dict[str, object]]] = defaultdict(list)

        for release in releases:
            for asset in release.assets:
                if not asset.name.endswith(".tgz"):
                    continue

                chart_metadata = self._parse_chart_filename(asset.name)
                chart_name = self._get_required_string(chart_metadata, "name", asset.name)
                chart_version = self._get_required_string(chart_metadata, "version", asset.name)

                entry = {
                    "apiVersion": "v2",
                    "name": chart_name,
                    "version": chart_version,
                    "type": "library",
                    "urls": [self._build_chart_url(asset)],
                    "created": self._format_datetime(release.published_at),
                    "digest": self._normalize_digest(asset.digest),
                }
                entries[chart_name].append({k: v for k, v in entry.items() if v is not None})

        for chart_entries in entries.values():
            chart_entries.sort(key=lambda item: str(item["version"]), reverse=True)

        document = {
            "apiVersion": "v1",
            "generated": self._format_datetime(datetime.now(UTC)),
            "entries": dict(entries),
        }
        return yaml.safe_dump(document, sort_keys=False)

    def _build_chart_url(self, asset: GitHubReleaseAsset) -> str:
        return f"charts/{asset.id}/{quote(asset.name, safe='')}"

    def _parse_chart_filename(self, filename: str) -> Mapping[str, str]:
        if not filename.endswith(".tgz"):
            raise IndexBuildError(f"{filename} is not a chart archive")

        stem = filename[:-4]
        match = CHART_FILENAME_PATTERN.match(stem)
        if match:
            return {"name": match.group("name"), "version": match.group("version")}

        raise IndexBuildError(
            f"{filename} does not match the required '<chart-name>-<version>.tgz' format"
        )

    def _normalize_digest(self, digest: str | None) -> str | None:
        if digest is None:
            return None
        if digest.startswith("sha256:"):
            return digest.removeprefix("sha256:")
        return None

    def _get_required_string(
        self, chart_metadata: Mapping[str, object], field_name: str, asset_name: str
    ) -> str:
        value = chart_metadata.get(field_name)
        if not isinstance(value, str) or not value.strip():
            raise IndexBuildError(f"{asset_name} is missing required chart field '{field_name}'")
        return value

    def _format_datetime(self, value: datetime | None) -> str | None:
        if value is None:
            return None
        return value.astimezone(UTC).isoformat().replace("+00:00", "Z")
