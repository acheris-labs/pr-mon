"""pr-mon: monitor and merge GitHub pull requests from the terminal."""

import logging
from importlib.metadata import version

__version__ = version("pr-mon")

# Library code logs; only the daemon decides where logs go (never onto the TUI's screen).
logging.getLogger(__name__).addHandler(logging.NullHandler())
