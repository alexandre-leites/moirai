from __future__ import annotations

from datetime import UTC, datetime, timedelta
from hashlib import sha256
import unittest

from moirai.domain.control_plane import AuthenticationError
from moirai.persistence.authentication import AsyncpgAuthentication, hash_password, verify_password


NOW = datetime(2026, 1, 1, tzinfo=UTC)
USER_ID = "00000000-0000-0000-0000-000000000001"


class _Transaction:
    async def __aenter__(self) -> _Transaction:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _Connection:
    def __init__(self, pool: _Pool) -> None:
        self.pool = pool

    async def __aenter__(self) -> _Connection:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "FROM app.users" in query and "WHERE username" in query:
            user = self.pool.users.get(str(arguments[0]))
            return None if user is None else dict(user)
        if "FROM app.user_sessions AS s" in query:
            session = self.pool.sessions.get(str(arguments[0]))
            if session is None or session["revoked_at"] is not None or session["expires_at"] <= arguments[1]:
                return None
            user = self.pool.users_by_id.get(str(session["user_id"]))
            if user is None or not user["enabled"]:
                return None
            return {
                "id": session["id"],
                "user_id": session["user_id"],
                "csrf_token_hash": session["csrf_token_hash"],
                "expires_at": session["expires_at"],
                "username": user["username"],
                "role": user["role"],
            }
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.user_sessions" in query:
            self.pool.sessions[str(arguments[2])] = {
                "id": arguments[0],
                "user_id": arguments[1],
                "csrf_token_hash": arguments[3],
                "created_at": arguments[4],
                "expires_at": arguments[5],
                "revoked_at": None,
            }
            return "INSERT 0 1"
        if "UPDATE app.users SET last_login_at" in query:
            self.pool.users_by_id[str(arguments[0])]["last_login_at"] = arguments[1]
            return "UPDATE 1"
        if "UPDATE app.user_sessions SET last_seen_at" in query:
            self.pool.last_seen_at = arguments[1]
            return "UPDATE 1"
        if "INSERT INTO app.audit_events" in query:
            self.pool.audit.append(arguments)
            return "INSERT 0 1"
        raise AssertionError(query)


class _Pool:
    def __init__(self) -> None:
        password_hash = hash_password("correct horse battery staple")
        user = {
            "id": USER_ID,
            "username": "admin",
            "password_hash": password_hash,
            "enabled": True,
            "role": "admin",
        }
        self.users = {"admin": user}
        self.users_by_id = {USER_ID: user}
        self.sessions: dict[str, dict[str, object]] = {}
        self.audit: list[tuple[object, ...]] = []
        self.last_seen_at: datetime | None = None

    def acquire(self) -> _Connection:
        return _Connection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        if "UPDATE app.user_sessions" not in query:
            raise AssertionError(query)
        session = self.sessions.get(str(arguments[0]))
        if session is not None and session["revoked_at"] is None:
            session["revoked_at"] = arguments[1]
        return "UPDATE 1"


class AuthenticationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.pool = _Pool()
        self.authentication = AsyncpgAuthentication(self.pool, session_ttl=timedelta(hours=1))

    def test_scrypt_password_hashes_are_salted_and_verify(self) -> None:
        first = hash_password("correct horse battery staple")
        second = hash_password("correct horse battery staple")
        self.assertNotEqual(first, second)
        self.assertTrue(verify_password("correct horse battery staple", first))
        self.assertFalse(verify_password("wrong horse battery staple", first))
        self.assertFalse(verify_password("correct horse battery staple", "plaintext"))

    async def test_login_stores_only_hashes_and_writes_audit_record(self) -> None:
        credentials = await self.authentication.login(" admin ", "correct horse battery staple", NOW)
        self.assertEqual(credentials.user_id, USER_ID)
        self.assertNotIn(credentials.session_token, self.pool.sessions)
        self.assertNotIn(credentials.csrf_token, self.pool.sessions)
        self.assertEqual(len(self.pool.audit), 1)
        self.assertNotIn(credentials.session_token, str(self.pool.audit[0]))
        self.assertNotIn(credentials.csrf_token, str(self.pool.audit[0]))

    async def test_login_rejects_wrong_password_and_disabled_user(self) -> None:
        with self.assertRaises(AuthenticationError):
            await self.authentication.login("admin", "wrong horse battery staple", NOW)
        self.pool.users["admin"]["enabled"] = False
        with self.assertRaises(AuthenticationError):
            await self.authentication.login("admin", "correct horse battery staple", NOW)

    async def test_session_requires_matching_csrf_and_honors_expiry(self) -> None:
        credentials = await self.authentication.login("admin", "correct horse battery staple", NOW)
        session = await self.authentication.validate_session(
            credentials.session_token, credentials.csrf_token, NOW + timedelta(minutes=1), True
        )
        self.assertEqual(session.user_id, USER_ID)
        self.assertEqual(session.role, "admin")
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                credentials.session_token, "wrong", NOW + timedelta(minutes=1), True
            )
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                credentials.session_token, credentials.csrf_token, NOW + timedelta(hours=2), True
            )

    async def test_revoked_session_cannot_be_reused(self) -> None:
        credentials = await self.authentication.login("admin", "correct horse battery staple", NOW)
        await self.authentication.revoke_session(credentials.session_token, NOW + timedelta(minutes=1))
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                credentials.session_token, credentials.csrf_token, NOW + timedelta(minutes=2), True
            )


if __name__ == "__main__":
    unittest.main()
