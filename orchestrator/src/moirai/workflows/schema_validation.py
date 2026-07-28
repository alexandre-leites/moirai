from __future__ import annotations

import json
from functools import cache
from pathlib import Path
from typing import Any

_SCHEMAS_DIR = Path(__file__).resolve().parents[4] / "schemas"

_TYPE_MAP: dict[str, type | tuple[type, ...]] = {
    "object": dict,
    "array": list,
    "string": str,
    "integer": int,
    "boolean": bool,
}


class SchemaNotFoundError(ValueError):
    pass


@cache
def load_schema(name: str) -> dict[str, Any]:
    path = _SCHEMAS_DIR / f"{name}.schema.json"
    try:
        contents = path.read_text("utf-8")
    except OSError as error:
        raise SchemaNotFoundError(f"schema {name!r} could not be read") from error
    return json.loads(contents)


def validate(payload: Any, schema: dict[str, Any]) -> list[str]:
    """Validates payload against the subset of JSON Schema draft 2020-12 used
    by schemas/*.schema.json (type, required, properties, items, enum, const).
    Returns a list of human-readable errors; empty means valid."""
    errors: list[str] = []
    _validate_node(payload, schema, "$", errors)
    return errors


def _validate_node(value: Any, schema: dict[str, Any], path: str, errors: list[str]) -> None:
    if "const" in schema:
        if value != schema["const"]:
            errors.append(f"{path} must equal {schema['const']!r}")
        return
    if "enum" in schema:
        if value not in schema["enum"]:
            errors.append(f"{path} must be one of {schema['enum']!r}")
        return
    expected_type = schema.get("type")
    if expected_type is not None:
        python_type = _TYPE_MAP.get(expected_type)
        if python_type is not None and not isinstance(value, python_type):
            errors.append(f"{path} must be of type {expected_type}")
            return
    if expected_type == "object" and isinstance(value, dict):
        for required in schema.get("required", []):
            if required not in value:
                errors.append(f"{path}.{required} is required")
        for key, subschema in schema.get("properties", {}).items():
            if key in value:
                _validate_node(value[key], subschema, f"{path}.{key}", errors)
    elif expected_type == "array" and isinstance(value, list):
        item_schema = schema.get("items")
        if item_schema is not None:
            for index, item in enumerate(value):
                _validate_node(item, item_schema, f"{path}[{index}]", errors)
