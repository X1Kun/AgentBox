from lib.core.errors import ToolError

def require(value, reason, detail):
    if not value:
        raise ToolError(reason, detail)
