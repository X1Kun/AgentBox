"""Stable operational failure categories shared by command layers."""

class ToolError(Exception):
    def __init__(self, reason, detail):
        super().__init__(detail)
        self.reason = reason


class LabError(ToolError):
    pass


class GVisorError(ToolError):
    pass


class NetworkError(ToolError):
    pass


class SessionError(ToolError):
    pass


class BatchError(ToolError):
    pass


class CommandError(LabError):
    pass
