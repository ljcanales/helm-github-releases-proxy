from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any


class TTLCache:
    def __init__(self) -> None:
        self._items: dict[str, tuple[datetime, Any]] = {}

    def get(self, key: str) -> Any | None:
        item = self._items.get(key)
        if item is None:
            return None

        expires_at, value = item
        if expires_at <= datetime.now(UTC):
            self._items.pop(key, None)
            return None
        return value

    def set(self, key: str, value: Any, ttl_seconds: int) -> Any:
        expires_at = datetime.now(UTC) + timedelta(seconds=ttl_seconds)
        self._items[key] = (expires_at, value)
        return value

    def get_or_set(self, key: str, factory: Callable[[], Any], ttl_seconds: int) -> Any:
        cached = self.get(key)
        if cached is not None:
            return cached
        value = factory()
        return self.set(key, value, ttl_seconds)


_cache = TTLCache()


def get_cache() -> TTLCache:
    return _cache
