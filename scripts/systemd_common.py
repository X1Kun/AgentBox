"""Shared fixed-service configuration parsing; never evaluates shell syntax."""

import ipaddress
import re
from pathlib import PurePosixPath
from urllib.parse import urlsplit


def load_config(path):
    values = {"AGENTBOX_PLATFORM": "kvm", "AGENTBOX_LISTEN": "127.0.0.1:8080", "AGENTBOX_CONCURRENCY": "1", "AGENTBOX_MODE": "cold",
              "AGENTBOX_WARM_SIZE": "3", "AGENTBOX_MAX_SANDBOXES": "6", "AGENTBOX_CREATE_CONCURRENCY": "1",
              "AGENTBOX_QUEUE_SIZE": "16", "AGENTBOX_QUEUE_BYTES": "8388608", "AGENTBOX_QUEUE_TIMEOUT": "100ms", "AGENTBOX_QOS": "off", "AGENTBOX_QOS_INTERVAL": "100ms",
              "AGENTBOX_QOS_PROTECTED_CGROUP": "none"}
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
    if values["AGENTBOX_MODE"] not in ("cold", "warm"):
        raise ValueError("mode must be cold or warm")
    bounds = {"AGENTBOX_WARM_SIZE": (1, 8), "AGENTBOX_MAX_SANDBOXES": (1, 8),
              "AGENTBOX_CREATE_CONCURRENCY": (1, 2), "AGENTBOX_QUEUE_SIZE": (1, 128),
              "AGENTBOX_QUEUE_BYTES": (1, 64 << 20)}
    for key, (minimum, maximum) in bounds.items():
        if not re.fullmatch(r"[0-9]+", values[key]) or not minimum <= int(values[key]) <= maximum:
            raise ValueError("invalid pool setting: "+key)
    if (int(values["AGENTBOX_WARM_SIZE"]) > int(values["AGENTBOX_MAX_SANDBOXES"])
            or int(values["AGENTBOX_CONCURRENCY"]) > int(values["AGENTBOX_MAX_SANDBOXES"])
            or int(values["AGENTBOX_CREATE_CONCURRENCY"]) > int(values["AGENTBOX_WARM_SIZE"])):
        raise ValueError("pool capacity settings are inconsistent")
    duration = re.fullmatch(r"([0-9]+)(ms|s)", values["AGENTBOX_QUEUE_TIMEOUT"])
    if not duration or not 1 <= int(duration[1]) * (1000 if duration[2] == "s" else 1) <= 5000:
        raise ValueError("queue timeout must be 1ms..5s")
    if values["AGENTBOX_QOS"] not in ("off", "observe"):
        raise ValueError("qos must be off or observe; enforcement is not implemented")
    interval = re.fullmatch(r"([0-9]+)(ms|s)", values["AGENTBOX_QOS_INTERVAL"])
    if not interval or not 50 <= int(interval[1]) * (1000 if interval[2] == "s" else 1) <= 1000:
        raise ValueError("qos interval must be 50ms..1s")
    if values["AGENTBOX_QOS"] == "observe":
        protected = PurePosixPath(values["AGENTBOX_QOS_PROTECTED_CGROUP"])
        if ".." in protected.parts or not str(protected).startswith("/sys/fs/cgroup/"):
            raise ValueError("observe requires a protected cgroup below /sys/fs/cgroup")
    url = urlsplit("http://" + values["AGENTBOX_LISTEN"])
    try:
        valid = ipaddress.ip_address(url.hostname).is_loopback and 1 <= url.port <= 65535
    except (ValueError, TypeError):
        valid = False
    if not valid or url.username or url.password or url.path or url.query or url.fragment:
        raise ValueError("listen must contain an explicit loopback IP and port")
    return values
