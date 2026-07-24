from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from base64 import urlsafe_b64decode, urlsafe_b64encode
from hashlib import scrypt
from hmac import compare_digest
import json
from secrets import token_bytes, token_urlsafe
from typing import Any
from uuid import uuid4

from moirai.domain.control_plane import AuthenticationError


_SCRYPT_N = 1 << 14
_SCRYPT_R = 8
_SCRYPT_P = 1
_DKLEN = 32


@dataclass(frozen=True)
class SessionCredentials:
    session_token: str
    csrf_token: str
    user_id: str
    expires_at: datetime


@dataclass(frozen=True)
class AuthenticatedSession:
    id: str
    user_id: str
    username: str
    role: str
    expires_at: datetime


def hash_password(password: str) -> str:
    _validate_password(password)
    salt = token_bytes(16)
    digest = scrypt(
        password.encode("utf-8"),
        salt=salt,
        n=_SCRYPT_N,
        r=_SCRYPT_R,
        p=_SCRYPT_P,
        dklen=_DKLEN,
    )
    return "$".join(
        (
            "scrypt",
            str(_SCRYPT_N),
            str(_SCRYPT_R),
            str(_SCRYPT_P),
            _encode(salt),
            _encode(digest),
        )
    )


def verify_password(password: str, encoded: str) -> bool:
    try:
        algorithm, n, r, p, salt, digest = encoded.split("$")
        if algorithm != "scrypt":
            return False
        parameters = (int(n), int(r), int(p))
        if parameters != (_SCRYPT_N, _SCRYPT_R, _SCRYPT_P):
            return False
        expected = _decode(digest)
        actual = scrypt(
            password.encode("utf-8"),
            salt=_decode(salt),
            n=parameters[0],
            r=parameters[1],
            p=parameters[2],
            dklen=len(expected),
        )
    except (TypeError, ValueError):
        return False
    return compare_digest(actual, expected)


class AsyncpgAuthentication:
    def __init__(self, pool: Any, session_ttl: timedelta = timedelta(days=7)) -> None:
        if session_ttl <= timedelta():
            raise ValueError("session TTL must be positive")
        self._pool = pool
        self._session_ttl = session_ttl

    async def create_user(
        self, username: str, password: str, now: datetime, role: str = "admin"
    ) -> str:
        normalized = _normalize_username(username)
        if role not in {"admin", "viewer"}:
            raise ValueError("user role is invalid")
        user_id = uuid4()
        try:
            await self._pool.execute(
                """
                INSERT INTO app.users (id, username, password_hash, role, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $5)
                """,
                user_id,
                normalized,
                hash_password(password),
                role,
                now,
            )
        except Exception as error:
            if _is_unique_violation(error):
                raise ValueError("username is already in use") from error
            raise
        return str(user_id)

    async def login(self, username: str, password: str, now: datetime) -> SessionCredentials:
        normalized = _normalize_username(username)
        if not password:
            raise AuthenticationError("login was rejected")
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                user = await connection.fetchrow(
                    """
                    SELECT id, password_hash, enabled
                    FROM app.users
                    WHERE username = $1
                    FOR UPDATE
                    """,
                    normalized,
                )
                if user is None or not bool(user["enabled"]) or not verify_password(
                    password, str(user["password_hash"])
                ):
                    raise AuthenticationError("login was rejected")
                credentials = self._new_session(str(user["id"]), now)
                await connection.execute(
                    """
                    INSERT INTO app.user_sessions
                        (id, user_id, token_hash, csrf_token_hash, created_at, expires_at, last_seen_at)
                    VALUES ($1, $2, $3, $4, $5, $6, $5)
                    """,
                    uuid4(),
                    user["id"],
                    _hash_token(credentials.session_token),
                    _hash_token(credentials.csrf_token),
                    now,
                    credentials.expires_at,
                )
                await connection.execute(
                    "UPDATE app.users SET last_login_at = $2, updated_at = $2 WHERE id = $1",
                    user["id"],
                    now,
                )
                await self._append_audit(
                    connection,
                    actor_user_id=user["id"],
                    action="user.login",
                    resource_type="user",
                    resource_id=str(user["id"]),
                    outcome="succeeded",
                    now=now,
                )
        return credentials

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> AuthenticatedSession:
        if not session_token or (require_csrf and not csrf_token):
            raise AuthenticationError("session is invalid")
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                row = await connection.fetchrow(
                    """
                    SELECT s.id, s.user_id, s.csrf_token_hash, s.expires_at, u.username, u.role
                    FROM app.user_sessions AS s
                    JOIN app.users AS u ON u.id = s.user_id
                    WHERE s.token_hash = $1
                      AND s.revoked_at IS NULL
                      AND s.expires_at > $2
                      AND u.enabled = true
                    FOR UPDATE
                    """,
                    _hash_token(session_token),
                    now,
                )
                if row is None or (
                    require_csrf
                    and not compare_digest(str(row["csrf_token_hash"]), _hash_token(str(csrf_token)))
                ):
                    raise AuthenticationError("session is invalid")
                await connection.execute(
                    "UPDATE app.user_sessions SET last_seen_at = $2 WHERE id = $1", row["id"], now
                )
        return AuthenticatedSession(
            id=str(row["id"]),
            user_id=str(row["user_id"]),
            username=str(row["username"]),
            role=str(row["role"]),
            expires_at=row["expires_at"],
        )

    async def revoke_session(self, session_token: str, now: datetime) -> None:
        if not session_token:
            return
        await self._pool.execute(
            """
            UPDATE app.user_sessions
            SET revoked_at = $2
            WHERE token_hash = $1 AND revoked_at IS NULL
            """,
            _hash_token(session_token),
            now,
        )

    async def append_audit(
        self,
        actor_user_id: str | None,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None:
        await self._append_audit(
            self._pool,
            actor_user_id=_uuid_or_none(actor_user_id),
            action=action,
            resource_type=resource_type,
            resource_id=resource_id,
            outcome=outcome,
            now=now,
        )

    def _new_session(self, user_id: str, now: datetime) -> SessionCredentials:
        return SessionCredentials(
            session_token=token_urlsafe(32),
            csrf_token=token_urlsafe(32),
            user_id=user_id,
            expires_at=now + self._session_ttl,
        )

    @staticmethod
    async def _append_audit(
        executor: Any,
        actor_user_id: Any,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None:
        if not action or not resource_type or not resource_id or not outcome:
            raise ValueError("audit record is invalid")
        await executor.execute(
            """
            INSERT INTO app.audit_events
                (actor_type, actor_id, action, target_type, target_id, payload, created_at)
            VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
            """,
            "user" if actor_user_id is not None else "system",
            str(actor_user_id) if actor_user_id is not None else None,
            action,
            resource_type,
            resource_id,
            json.dumps({"outcome": outcome}, separators=(",", ":")),
            now,
        )


def _hash_token(token: str) -> str:
    from hashlib import sha256

    return sha256(token.encode("utf-8")).hexdigest()


def _normalize_username(username: str) -> str:
    normalized = username.strip()
    if not normalized or len(normalized) > 128:
        raise AuthenticationError("login was rejected")
    return normalized


def _validate_password(password: str) -> None:
    if not isinstance(password, str) or len(password) < 12 or len(password) > 1024:
        raise ValueError("password must contain between 12 and 1024 characters")


def _encode(value: bytes) -> str:
    return urlsafe_b64encode(value).decode("ascii").rstrip("=")


def _decode(value: str) -> bytes:
    return urlsafe_b64decode(value + "=" * (-len(value) % 4))


def _uuid_or_none(value: str | None) -> Any:
    from uuid import UUID

    return UUID(value) if value is not None else None


def _is_unique_violation(error: Exception) -> bool:
    return getattr(error, "sqlstate", None) == "23505"
