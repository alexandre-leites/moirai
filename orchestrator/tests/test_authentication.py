from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta
from typing import Self
from unittest.mock import patch

from moirai.domain.control_plane import AuthenticationError
from moirai.persistence.authentication import AsyncpgAuthentication, hash_password, verify_password

NOW = datetime(2026, 1, 1, tzinfo=UTC)
USER_ID = "00000000-0000-0000-0000-000000000001"


class _Transaction:
    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _Connection:
    def __init__(self, pool: _Pool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "FROM app.users" in query and "WHERE username" in query:
            user = self.pool.users.get(str(arguments[0]))
            return None if user is None else dict(user)
        if "FROM app.users" in query and "FOR UPDATE" in query:
            user = self.pool.users_by_id.get(str(arguments[0]))
            return None if user is None else dict(user)
        if "FROM app.user_sessions AS s" in query:
            self.pool.session_lookups += 1
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
                "last_seen_at": session["last_seen_at"],
                "username": user["username"],
                "role": user["role"],
                "email": user["email"],
                "display_name": user["display_name"],
            }
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.user_sessions" in query:
            session = {
                "id": arguments[0],
                "user_id": arguments[1],
                "csrf_token_hash": arguments[3],
                "created_at": arguments[4],
                "expires_at": arguments[5],
                "revoked_at": None,
                "last_seen_at": arguments[4],
            }
            self.pool.sessions[str(arguments[2])] = session
            self.pool.sessions_by_id[str(arguments[0])] = session
            return "INSERT 0 1"
        if "UPDATE app.users SET last_login_at" in query:
            self.pool.users_by_id[str(arguments[0])]["last_login_at"] = arguments[1]
            return "UPDATE 1"
        if "UPDATE app.user_sessions SET last_seen_at" in query:
            self.pool.last_seen_at_writes += 1
            session = self.pool.sessions_by_id.get(str(arguments[0]))
            if session is not None:
                session["last_seen_at"] = arguments[1]
            return "UPDATE 1"
        if "UPDATE app.user_sessions" in query and "WHERE user_id = $1" in query:
            exclude = str(arguments[1]) if "id != $2" in query else None
            revoked_at = arguments[2] if "id != $2" in query else arguments[1]
            for session in self.pool.sessions.values():
                if (
                    str(session["user_id"]) == str(arguments[0])
                    and session["revoked_at"] is None
                    and (exclude is None or str(session["id"]) != exclude)
                ):
                    session["revoked_at"] = revoked_at
            return "UPDATE 1"
        if "UPDATE app.users SET password_hash" in query:
            user = self.pool.users_by_id[str(arguments[0])]
            user["password_hash"] = arguments[1]
            return "UPDATE 1"
        if "UPDATE app.users SET email" in query:
            user = self.pool.users_by_id[str(arguments[0])]
            user["email"] = arguments[1]
            user["display_name"] = arguments[2]
            return "UPDATE 1"
        if "INSERT INTO app.audit_events" in query:
            self.pool.audit.append(arguments)
            return "INSERT 0 1"
        raise AssertionError(query)


class _Pool:
    def __init__(self) -> None:
        password_hash = hash_password("Correct horse battery staple 9!")
        user = {
            "id": USER_ID,
            "username": "admin",
            "password_hash": password_hash,
            "enabled": True,
            "role": "admin",
            "email": "",
            "display_name": "",
        }
        self.users = {"admin": user}
        self.users_by_id = {USER_ID: user}
        self.sessions: dict[str, dict[str, object]] = {}
        self.sessions_by_id: dict[str, dict[str, object]] = {}
        self.audit: list[tuple[object, ...]] = []
        self.workflow_events: list[dict[str, object]] = []
        self.last_seen_at_writes = 0
        self.session_lookups = 0

    def acquire(self) -> _Connection:
        return _Connection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        if "DELETE FROM app.user_sessions" in query:
            cutoff = arguments[0]
            kept: dict[str, dict[str, object]] = {}
            for token_hash, session in self.sessions.items():
                expired = session["expires_at"] < cutoff
                revoked_stale = session["revoked_at"] is not None and session["revoked_at"] < cutoff
                if expired or revoked_stale:
                    self.sessions_by_id.pop(str(session["id"]), None)
                    continue
                kept[token_hash] = session
            removed = len(self.sessions) - len(kept)
            self.sessions = kept
            return f"DELETE {removed}"
        if "DELETE FROM app.audit_events" in query:
            cutoff = arguments[0]
            before = len(self.audit)
            self.audit = [record for record in self.audit if record[6] >= cutoff]
            return f"DELETE {before - len(self.audit)}"
        if "DELETE FROM app.workflow_events" in query:
            cutoff = arguments[0]
            before = len(self.workflow_events)
            self.workflow_events = [e for e in self.workflow_events if e["created_at"] >= cutoff]
            return f"DELETE {before - len(self.workflow_events)}"
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
        first = hash_password("Correct horse battery staple 9!")
        second = hash_password("Correct horse battery staple 9!")
        self.assertNotEqual(first, second)
        self.assertTrue(verify_password("Correct horse battery staple 9!", first))
        self.assertFalse(verify_password("wrong horse battery staple", first))
        self.assertFalse(verify_password("Correct horse battery staple 9!", "plaintext"))

    async def test_login_stores_only_hashes_and_writes_audit_record(self) -> None:
        credentials = await self.authentication.login(" admin ", "Correct horse battery staple 9!", NOW)
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
            await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)

    async def test_login_pays_the_same_kdf_cost_for_unknown_usernames(self) -> None:
        with patch(
            "moirai.persistence.authentication.verify_password", wraps=verify_password
        ) as spy, self.assertRaises(AuthenticationError):
            await self.authentication.login("nobody-registered", "whatever password", NOW)
        spy.assert_called_once()
        # A nonexistent username must still run verify_password (paying the full KDF
        # cost) rather than short-circuiting before it — otherwise login latency reveals
        # whether the username exists.
        called_password, called_hash = spy.call_args.args
        self.assertEqual(called_password, "whatever password")
        self.assertNotEqual(called_hash, "")

    async def test_login_revokes_the_previous_session_for_the_user(self) -> None:
        first = await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
        second = await self.authentication.login(
            "admin", "Correct horse battery staple 9!", NOW + timedelta(minutes=1)
        )
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                first.session_token, first.csrf_token, NOW + timedelta(minutes=2), True
            )
        session = await self.authentication.validate_session(
            second.session_token, second.csrf_token, NOW + timedelta(minutes=2), True
        )
        self.assertEqual(session.user_id, USER_ID)

    async def test_session_requires_matching_csrf_and_honors_expiry(self) -> None:
        credentials = await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
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

    async def test_validate_session_throttles_last_seen_at_writes(self) -> None:
        credentials = await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
        self.pool.last_seen_at_writes = 0
        await self.authentication.validate_session(
            credentials.session_token, credentials.csrf_token, NOW + timedelta(seconds=1), True
        )
        self.assertEqual(self.pool.last_seen_at_writes, 0, "a request 1s later must not re-write last_seen_at")
        await self.authentication.validate_session(
            credentials.session_token, credentials.csrf_token, NOW + timedelta(minutes=5), True
        )
        self.assertEqual(self.pool.last_seen_at_writes, 1, "a stale last_seen_at must be refreshed")

    async def test_update_account_changes_password_and_revokes_other_sessions(self) -> None:
        await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
        session_id = next(iter(self.pool.sessions_by_id))
        other = dict(self.pool.sessions_by_id[session_id])
        other["id"] = "00000000-0000-0000-0000-000000000099"
        self.pool.sessions_by_id[str(other["id"])] = other
        self.pool.sessions["other-token-hash"] = other
        profile = await self.authentication.update_account(
            USER_ID,
            keep_session_id=session_id,
            current_password="Correct horse battery staple 9!",
            new_password="Correct horse battery staple 10!",
            new_email="admin@example.com",
            display_name="Admin User",
            now=NOW + timedelta(minutes=1),
        )
        self.assertEqual(profile.email, "admin@example.com")
        self.assertEqual(profile.display_name, "Admin User")
        self.assertTrue(verify_password("Correct horse battery staple 10!", self.pool.users["admin"]["password_hash"]))
        self.assertEqual(len(self.pool.audit), 2, "login and password-change audit records")
        self.assertEqual(self.pool.audit[1][2], "user.password_changed")
        revoked = [s for s in self.pool.sessions_by_id.values() if s.get("revoked_at") is not None]
        self.assertEqual(len(revoked), 1, "only the other session is revoked")

    async def test_update_account_rejects_wrong_current_password(self) -> None:
        with self.assertRaises(AuthenticationError):
            await self.authentication.update_account(
                USER_ID,
                keep_session_id="00000000-0000-0000-0000-000000000001",
                current_password="wrong password",
                new_password="Correct horse battery staple 10!",
                new_email="",
                display_name="",
                now=NOW + timedelta(minutes=1),
            )

    async def test_update_account_profile_only_leaves_password_unchanged(self) -> None:
        profile = await self.authentication.update_account(
            USER_ID,
            keep_session_id="00000000-0000-0000-0000-000000000001",
            current_password="",
            new_password="",
            new_email=" admin@example.com ",
            display_name="  ",
            now=NOW + timedelta(minutes=1),
        )
        self.assertEqual(profile.email, "admin@example.com")
        self.assertEqual(self.pool.audit[0][2], "user.profile_updated")

    async def test_revoked_session_cannot_be_reused(self) -> None:
        credentials = await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
        await self.authentication.revoke_session(credentials.session_token, NOW + timedelta(minutes=1))
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                credentials.session_token, credentials.csrf_token, NOW + timedelta(minutes=2), True
            )

    async def test_reap_expired_sessions_deletes_past_grace_period(self) -> None:
        await self.authentication.login("admin", "Correct horse battery staple 9!", NOW)
        removed = await self.authentication.reap_expired_sessions(NOW + timedelta(minutes=30))
        self.assertEqual(removed, 0, "session has not expired yet")
        removed = await self.authentication.reap_expired_sessions(NOW + timedelta(days=3))
        self.assertEqual(removed, 1)
        self.assertEqual(self.pool.sessions, {})

    async def test_reap_audit_and_workflow_events_respects_retention(self) -> None:
        self.pool.audit.append(("system", None, "user.login", "user", USER_ID, "{}", NOW))
        self.pool.workflow_events.append({"created_at": NOW})
        removed_audit = await self.authentication.reap_audit_events(
            NOW + timedelta(days=1), retention=timedelta(days=90)
        )
        self.assertEqual(removed_audit, 0)
        removed_audit = await self.authentication.reap_audit_events(
            NOW + timedelta(days=91), retention=timedelta(days=90)
        )
        self.assertEqual(removed_audit, 1)
        removed_workflow = await self.authentication.reap_workflow_events(
            NOW + timedelta(days=31), retention=timedelta(days=30)
        )
        self.assertEqual(removed_workflow, 1)


if __name__ == "__main__":
    unittest.main()
