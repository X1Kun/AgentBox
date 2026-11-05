"""Repository and lab identities; no command execution."""

from pathlib import Path
import re
ROOT = Path(__file__).resolve().parents[3]
CLUSTER = 'agentbox-platform-lab'
CONTEXT = 'kind-' + CLUSTER
NAMESPACE = 'agentbox-platform'
OWNER_LABEL = 'agentbox.dev/session'
HEX64 = re.compile(r'^[0-9a-f]{64}$')
GVISOR_CASES = ('success','failure','cancel','deadline','oom','missing')
