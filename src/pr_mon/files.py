"""XDG locations and safe file writes."""

import os
import tempfile
from pathlib import Path

APP_NAME = "pr-mon"


def app_dir(env_var: str, fallback: Path) -> Path:
    base = os.environ.get(env_var)
    return (Path(base) if base else fallback) / APP_NAME


def write_atomic(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise
