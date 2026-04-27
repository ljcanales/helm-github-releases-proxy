import logging

from fastapi import FastAPI

from app.config import get_settings
from app.routes.health import router as health_router
from app.routes.repo import router as repo_router


def create_app() -> FastAPI:
    settings = get_settings()

    logging.getLogger("uvicorn").propagate = False
    logging.basicConfig(level=settings.log_level.upper())

    app = FastAPI(
        title="helm-github-releases-proxy",
        version="0.1.0",
        description="Helm chart repository proxy backed by GitHub Releases.",
    )
    app.include_router(health_router)
    app.include_router(repo_router)
    return app


app = create_app()
