"""Shared fixed-service configuration parsing; never evaluates shell syntax."""

import ipaddress
import re
from pathlib import PurePosixPath
from urllib.parse import urlsplit


def validate_loopback(value):
    url = urlsplit("http://" + value)
    try:
        valid = ipaddress.ip_address(url.hostname).is_loopback and 1 <= url.port <= 65535
    except (ValueError, TypeError):
        valid = False
    if not valid or url.username or url.password or url.path or url.query or url.fragment:
        raise ValueError("listen must contain an explicit loopback IP and port")


def load_gateway_config(path):
    values = read_environment(path, {"AGENTBOX_GATEWAY_LISTEN": "127.0.0.1:8080",
        "AGENTBOX_GATEWAY_MAX_INFLIGHT": "8", "AGENTBOX_GATEWAY_TIMEOUT": "50s",
        "AGENTBOX_GATEWAY_TOKEN": "", "AGENTBOX_DAEMON_TOKEN": ""})
    for key in ("AGENTBOX_GATEWAY_TOKEN", "AGENTBOX_DAEMON_TOKEN"):
        if not 32 <= len(values[key]) <= 4096 or any(not 33 <= ord(char) <= 126 for char in values[key]):
            raise ValueError("Gateway tokens must contain 32..4096 visible ASCII characters")
    if values["AGENTBOX_GATEWAY_TOKEN"] == values["AGENTBOX_DAEMON_TOKEN"]:
        raise ValueError("Gateway and daemon tokens must differ")
    validate_loopback(values["AGENTBOX_GATEWAY_LISTEN"])
    count = values["AGENTBOX_GATEWAY_MAX_INFLIGHT"]
    if not re.fullmatch(r"[0-9]+", count) or not 1 <= int(count) <= 64:
        raise ValueError("Gateway max-inflight must be 1..64")
    duration = re.fullmatch(r"([0-9]+)(ms|s)", values["AGENTBOX_GATEWAY_TIMEOUT"])
    if not duration or not 1 <= int(duration[1])*(1000 if duration[2] == "s" else 1) <= 60000:
        raise ValueError("Gateway timeout must be 1ms..60s")
    return values


def read_environment(path, defaults):
    values = dict(defaults)
    allowed = set(values)
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
    return values


def load_config(path):
    values = {"AGENTBOX_PLATFORM": "kvm", "AGENTBOX_LISTEN": "127.0.0.1:8080", "AGENTBOX_CONCURRENCY": "1", "AGENTBOX_MODE": "cold",
              "AGENTBOX_PARENT_MEMORY_MIB": "0", "AGENTBOX_SANDBOX_MEMORY_MIB": "512", "AGENTBOX_MEMORY_RESERVE_MIB": "512",
              "AGENTBOX_WARM_SIZE": "3", "AGENTBOX_MAX_SANDBOXES": "6", "AGENTBOX_CREATE_CONCURRENCY": "1",
              "AGENTBOX_WARM_DYNAMIC": "false", "AGENTBOX_WARM_MIN_SIZE": "1", "AGENTBOX_WARM_MAX_SIZE": "0",
              "AGENTBOX_WARM_ADJUST_INTERVAL": "1s", "AGENTBOX_WARM_DOWNSCALE_WINDOW": "30s",
              "AGENTBOX_QUEUE_SIZE": "16", "AGENTBOX_QUEUE_BYTES": "8388608", "AGENTBOX_QUEUE_TIMEOUT": "100ms", "AGENTBOX_QOS": "off", "AGENTBOX_QOS_INTERVAL": "100ms",
              "AGENTBOX_QOS_BASE_CPU": "0", "AGENTBOX_QOS_BUSINESS_FILE": "none",
              "AGENTBOX_QOS_PROTECTED_CGROUP": "none", "AGENTBOX_QOS_CPU_CONTROL": "false", "AGENTBOX_QOS_SCHEDULING": "false"}
    values["AGENTBOX_API_TOKEN"] = ""
    values = read_environment(path, values)
    if not 32 <= len(values.get("AGENTBOX_API_TOKEN", "")) <= 4096:
        raise ValueError("configuration requires an API token of 32 to 4096 characters")
    if values["AGENTBOX_PLATFORM"] not in ("kvm", "systrap"):
        raise ValueError("platform must be kvm or systrap")
    if values["AGENTBOX_CONCURRENCY"] not in ("1", "2", "3", "4"):
        raise ValueError("concurrency must be between 1 and 4")
    if values["AGENTBOX_MODE"] not in ("cold", "warm"):
        raise ValueError("mode must be cold or warm")
    if values["AGENTBOX_WARM_DYNAMIC"] not in ("true", "false"):
        raise ValueError("warm dynamic switch must be true or false")
    dynamic = values["AGENTBOX_WARM_DYNAMIC"] == "true"
    if dynamic and values["AGENTBOX_MODE"] != "warm":
        raise ValueError("dynamic inventory requires warm mode")
    bounds = {"AGENTBOX_PARENT_MEMORY_MIB": (0, 1<<30), "AGENTBOX_SANDBOX_MEMORY_MIB": (128, 1<<30), "AGENTBOX_MEMORY_RESERVE_MIB": (128, 1<<30),
              "AGENTBOX_WARM_SIZE": (1, 8), "AGENTBOX_MAX_SANDBOXES": (1, 8),
              "AGENTBOX_CREATE_CONCURRENCY": (1, 2), "AGENTBOX_QUEUE_SIZE": (1, 128),
              "AGENTBOX_QUEUE_BYTES": (1, 64 << 20)}
    for key, (minimum, maximum) in bounds.items():
        if not re.fullmatch(r"[0-9]+", values[key]) or not minimum <= int(values[key]) <= maximum:
            raise ValueError("invalid pool setting: "+key)
    max_inventory = int(values["AGENTBOX_WARM_SIZE"])
    if dynamic:
        for key, minimum in (("AGENTBOX_WARM_MIN_SIZE", 1), ("AGENTBOX_WARM_MAX_SIZE", 0)):
            if not re.fullmatch(r"[0-9]+", values[key]) or not minimum <= int(values[key]) <= 8:
                raise ValueError("invalid dynamic inventory bound: "+key)
        max_inventory = int(values["AGENTBOX_WARM_MAX_SIZE"]) or max_inventory
        if not int(values["AGENTBOX_WARM_MIN_SIZE"]) <= max_inventory <= int(values["AGENTBOX_MAX_SANDBOXES"]):
            raise ValueError("dynamic inventory bounds are inconsistent")
        durations = {}
        for key in ("AGENTBOX_WARM_ADJUST_INTERVAL", "AGENTBOX_WARM_DOWNSCALE_WINDOW"):
            duration = re.fullmatch(r"([0-9]+)(ms|s)", values[key])
            if not duration:
                raise ValueError("dynamic inventory durations must use integer ms or s")
            durations[key] = int(duration[1]) * (1000 if duration[2] == "s" else 1)
        interval = durations["AGENTBOX_WARM_ADJUST_INTERVAL"]
        if not 100 <= interval <= 60000 or not interval <= durations["AGENTBOX_WARM_DOWNSCALE_WINDOW"] <= 3600000:
            raise ValueError("invalid dynamic inventory timing")
    if ((values["AGENTBOX_MODE"] == "warm" and max_inventory > int(values["AGENTBOX_MAX_SANDBOXES"]))
            or int(values["AGENTBOX_CONCURRENCY"]) > int(values["AGENTBOX_MAX_SANDBOXES"])):
        raise ValueError("pool capacity settings are inconsistent")
    duration = re.fullmatch(r"([0-9]+)(ms|s)", values["AGENTBOX_QUEUE_TIMEOUT"])
    if not duration or not 1 <= int(duration[1]) * (1000 if duration[2] == "s" else 1) <= 5000:
        raise ValueError("queue timeout must be 1ms..5s")
    if values["AGENTBOX_QOS"] not in ("off", "observe", "enforce"):
        raise ValueError("qos must be off, observe or enforce")
    if any(values[key] not in ("true", "false") for key in ("AGENTBOX_QOS_CPU_CONTROL", "AGENTBOX_QOS_SCHEDULING")):
        raise ValueError("QoS policy switches must be true or false")
    if values["AGENTBOX_QOS"] == "off" and any(values[key] == "true" for key in ("AGENTBOX_QOS_CPU_CONTROL", "AGENTBOX_QOS_SCHEDULING")):
        raise ValueError("QoS policies require observe or enforce mode")
    interval = re.fullmatch(r"([0-9]+)(ms|s)", values["AGENTBOX_QOS_INTERVAL"])
    if not interval or not 50 <= int(interval[1]) * (1000 if interval[2] == "s" else 1) <= 1000:
        raise ValueError("qos interval must be 50ms..1s")
    base = values["AGENTBOX_QOS_BASE_CPU"]
    if not re.fullmatch(r"[0-9]+(?:\.[0-9]+)?", base) or not (float(base) == 0 or .5 <= float(base) <= 4):
        raise ValueError("service base CPU must be 0 or .5..4")
    health = values["AGENTBOX_QOS_BUSINESS_FILE"]
    if (float(base) > 0) != (health != "none"):
        raise ValueError("base CPU and business file must be configured together")
    if health != "none":
        path = PurePosixPath(health)
        if not path.is_absolute() or ".." in path.parts or str(path) != health:
            raise ValueError("business file must be a clean absolute path")
        if values["AGENTBOX_QOS"] == "off" or (values["AGENTBOX_QOS"] == "observe" and values["AGENTBOX_QOS_CPU_CONTROL"] != "true"):
            raise ValueError("business guard requires CPU control")
    if values["AGENTBOX_QOS"] != "off":
        protected = PurePosixPath(values["AGENTBOX_QOS_PROTECTED_CGROUP"])
        if ".." in protected.parts or not str(protected).startswith("/sys/fs/cgroup/"):
            raise ValueError("observe requires a protected cgroup below /sys/fs/cgroup")
    address = values["AGENTBOX_LISTEN"]
    if address.startswith("unix:"):
        socket = address[5:]
        if not socket.startswith("/") or str(PurePosixPath(socket)) != socket or ".." in PurePosixPath(socket).parts or len(socket.encode()) > 100:
            raise ValueError("listen requires a clean absolute Unix socket path of at most 100 bytes")
    else:
        validate_loopback(address)
    return values
