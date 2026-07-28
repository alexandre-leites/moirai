# `proto` is a runtime alias for moirai.protocols.proto (see proto/__init__.py's
# __path__ rewrite) that mypy cannot follow since it never executes module code.
# Re-export the generated stub here so static analysis sees the same types.
from moirai.protocols.proto.control_plane_pb2 import *
