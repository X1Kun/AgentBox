"""Shared fixed-service configuration parsing; never evaluates shell syntax."""

import ipaddress
from urllib.parse import urlsplit


def load_config(path):
    values = {"AGENTBOX_PLATFORM": "kvm", "AGENTBOX_LISTEN": "127.0.0.1:8080", "AGENTBOX_CONCURRENCY": "1"}
    allowed = {*values, "AGENTBOX_API_TOKEN"}
    seen = set()
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or key not in allowed or key in seen:
            raise ValueError("configuration requires unique supported KEY=VALUE entries")
        if not value or any(character.isspace() or character in "'\"\\\x00" for character in value):
            raise ValueError("configuration values must be unquoted and contain no whitespace or escapes")
        seen.add(key)
        values[key] = value
    if not 32 <= len(values.get("AGENTBOX_API_TOKEN", "")) <= 4096:
        raise ValueError("configuration requires an API token of 32 to 4096 characters")
    if values["AGENTBOX_PLATFORM"] not in ("kvm", "systrap"):
        raise ValueError("platform must be kvm or systrap")
    if values["AGENTBOX_CONCURRENCY"] not in ("1", "2", "3", "4"):
        raise ValueError("concurrency must be between 1 and 4")
    url = urlsplit("http://" + values["AGENTBOX_LISTEN"])
    try:
        valid = ipaddress.ip_address(url.hostname).is_loopback and 1 <= url.port <= 65535
    except (ValueError, TypeError):
        valid = False
    if not valid or url.username or url.password or url.path or url.query or url.fragment:
        raise ValueError("listen must contain an explicit loopback IP and port")
    return values
