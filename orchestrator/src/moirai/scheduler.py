from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from datetime import datetime, timedelta
import inspect
from typing import Any

from moirai.domain.control_plane import ScheduledJob


async def _await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class OfferDeliveryError(RuntimeError):
    pass


class AsyncpgLeader:
    def __init__(self, pool: Any, lock_id: int) -> None:
        self._pool = pool
        self._lock_id = lock_id
        self._connection: Any | None = None
        self._lease: Any | None = None
        self._held = False
        self._leadership_epoch = 0

    @property
    def leadership_epoch(self) -> int:
        return self._leadership_epoch

    async def is_leader(self) -> bool:
        if self._connection is None:
            self._lease = self._pool.acquire()
            self._connection = await self._lease.__aenter__()
        try:
            acquired = await self._connection.fetchval("SELECT pg_try_advisory_lock($1)", self._lock_id)
        except Exception:
            await self.close()
            raise
        if bool(acquired) and not self._held:
            self._leadership_epoch += 1
        self._held = bool(acquired)
        return self._held

    async def close(self) -> None:
        if self._connection is None:
            return
        try:
            if self._held:
                await self._connection.execute("SELECT pg_advisory_unlock($1)", self._lock_id)
        finally:
            await self._lease.__aexit__(None, None, None)
            self._connection = None
            self._lease = None
            self._held = False


class Scheduler:
    def __init__(
        self,
        control_plane: Any,
        deliver_offer: Callable[[Any, dict[str, Any]], Awaitable[bool]],
        task_packet: Callable[[ScheduledJob], Awaitable[dict[str, Any]] | dict[str, Any]],
        offer_ttl: timedelta,
    ) -> None:
        if offer_ttl <= timedelta():
            raise ValueError("offer TTL must be positive")
        self._control_plane = control_plane
        self._deliver_offer = deliver_offer
        self._task_packet = task_packet
        self._offer_ttl = offer_ttl

    async def tick(self, now: datetime) -> ScheduledJob | None:
        expire_offers = getattr(self._control_plane, "expire_offers", None)
        if expire_offers is not None:
            await _await(expire_offers(now))
        expire_leases = getattr(self._control_plane, "expire_leases", None)
        if expire_leases is not None:
            await _await(expire_leases(now))
        schedule_execution = getattr(self._control_plane, "schedule_execution", None)
        scheduled = (
            await _await(schedule_execution(now, self._offer_ttl))
            if schedule_execution is not None
            else None
        )
        if scheduled is None:
            recover_one = getattr(self._control_plane, "recover_one", None)
            scheduled = await _await(recover_one(now, self._offer_ttl)) if recover_one is not None else None
        if scheduled is None:
            scheduled = await _await(self._control_plane.schedule(now, self._offer_ttl))
        if scheduled is None:
            return None
        try:
            task_packet = await _await(self._task_packet(scheduled))
            delivered = await self._deliver_offer(scheduled.offer, task_packet)
        except Exception as error:
            await self._reject_offer(scheduled, now, error)
            raise OfferDeliveryError("scheduled offer delivery failed") from error
        if delivered:
            return scheduled
        await self._reject_offer(scheduled, now)
        return None

    async def _reject_offer(
        self, scheduled: ScheduledJob, now: datetime, delivery_error: Exception | None = None
    ) -> None:
        try:
            await _await(self._control_plane.reject_offer(scheduled.offer.job_id, scheduled.offer.runner_id, now))
        except Exception as cleanup_error:
            if delivery_error is None:
                raise OfferDeliveryError("scheduled offer rejection failed") from cleanup_error
            raise OfferDeliveryError("scheduled offer delivery and cleanup failed") from cleanup_error

    async def run(
        self,
        stop_event: asyncio.Event,
        now: Callable[[], datetime],
        interval: timedelta,
        is_leader: Callable[[], Awaitable[bool] | bool],
    ) -> None:
        if interval <= timedelta():
            raise ValueError("scheduler interval must be positive")
        while not stop_event.is_set():
            if await _await(is_leader()):
                await self.tick(now())
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
            except TimeoutError:
                pass

    async def run_with_leader(
        self,
        stop_event: asyncio.Event,
        now: Callable[[], datetime],
        interval: timedelta,
        leader: AsyncpgLeader,
    ) -> None:
        try:
            await self.run(stop_event, now, interval, leader.is_leader)
        finally:
            await leader.close()
