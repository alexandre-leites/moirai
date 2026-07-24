from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import Any
from uuid import uuid4


@dataclass
class RunnerSession:
    runner_id: str
    token: str = field(default_factory=lambda: str(uuid4()))
    _messages: asyncio.Queue[Any | None] = field(default_factory=asyncio.Queue)
    _offered_job_id: str | None = None
    _closed: bool = False

    @property
    def offered_job_id(self) -> str | None:
        return self._offered_job_id

    async def next_message(self) -> Any | None:
        return await self._messages.get()

    def deliver_offer(self, job_id: str, message: Any) -> bool:
        if self._closed or self._offered_job_id is not None:
            return False
        self._offered_job_id = job_id
        self._messages.put_nowait(message)
        return True

    def deliver_message(self, message: Any) -> bool:
        if self._closed:
            return False
        self._messages.put_nowait(message)
        return True

    def clear_offer(self, job_id: str) -> bool:
        if self._offered_job_id != job_id:
            return False
        self._offered_job_id = None
        return True

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            self._messages.put_nowait(None)


class RunnerSessionRegistry:
    def __init__(self) -> None:
        self._sessions: dict[str, RunnerSession] = {}
        self._lock = asyncio.Lock()

    async def connect(self, runner_id: str) -> RunnerSession:
        session = RunnerSession(runner_id)
        async with self._lock:
            previous = self._sessions.get(runner_id)
            self._sessions[runner_id] = session
        if previous is not None:
            previous.close()
        return session

    async def disconnect(self, session: RunnerSession) -> None:
        async with self._lock:
            if self._sessions.get(session.runner_id) is session:
                del self._sessions[session.runner_id]
        session.close()

    async def deliver_offer(self, runner_id: str, job_id: str, message: Any) -> bool:
        async with self._lock:
            session = self._sessions.get(runner_id)
            return session is not None and session.deliver_offer(job_id, message)

    async def clear_offer(self, runner_id: str, job_id: str) -> bool:
        async with self._lock:
            session = self._sessions.get(runner_id)
            return session is not None and session.clear_offer(job_id)

    async def deliver_message(self, runner_id: str, message: Any) -> bool:
        async with self._lock:
            session = self._sessions.get(runner_id)
            return session is not None and session.deliver_message(message)

    async def connected(self, runner_id: str) -> bool:
        async with self._lock:
            return runner_id in self._sessions
