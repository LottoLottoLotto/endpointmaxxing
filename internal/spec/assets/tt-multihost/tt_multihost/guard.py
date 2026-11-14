"""Checks on what a request may bring, kept free of device imports so they can be run anywhere.

The server listens on a port published on the host. Besides emx, a web page open in a browser on that
host can reach it, so requests a page could forge are refused:

- a Host that is a name other than localhost: a page that re-points its own domain at 127.0.0.1 (DNS
  rebinding) still sends its domain as Host;
- a POST that is not application/json: that is the one content type a page cannot send across origins
  without a preflight request, which this server never approves.
"""

from __future__ import annotations

import ipaddress

MAX_BODY_BYTES = 64 << 20

# Arguments a request may pass to the tokenizer's chat template. Anything else (above all `chat_template`,
# which would make the server render a template the client wrote) is refused.
TEMPLATE_KWARGS = {"enable_thinking": bool, "thinking": bool}


class BadRequest(Exception):
    pass


def host_allowed(host: str | None) -> bool:
    """True for localhost or an IP address (with or without a port); False for any other name."""
    if not host:
        return True  # HTTP/1.0 clients; a browser always sends Host
    name = host.strip()
    if name.startswith("["):  # [::1]:8000
        name = name[1:].split("]", 1)[0]
    elif name.count(":") == 1:
        name = name.rsplit(":", 1)[0]
    if name.lower() == "localhost":
        return True
    try:
        ipaddress.ip_address(name)
    except ValueError:
        return False
    return True


def is_json(content_type: str | None) -> bool:
    return (content_type or "").split(";", 1)[0].strip().lower() == "application/json"


def template_kwargs(raw) -> dict:
    """The chat_template_kwargs of a request, validated."""
    if raw is None:
        return {}
    if not isinstance(raw, dict):
        raise BadRequest("chat_template_kwargs must be an object")
    out = {}
    for key, value in raw.items():
        want = TEMPLATE_KWARGS.get(key)
        if want is None:
            raise BadRequest(f"chat_template_kwargs.{key} is not accepted (allowed: {', '.join(sorted(TEMPLATE_KWARGS))})")
        if not isinstance(value, want):
            raise BadRequest(f"chat_template_kwargs.{key} must be {want.__name__}")
        out[key] = value
    return out
