from __future__ import annotations

import asyncio
import inspect
import logging
from collections.abc import Awaitable, Callable
from datetime import datetime, timedelta
from typing import Any

from moirai.domain.control_plane import ScheduledJob

_LOGGER = logging.getLogger(__name__)


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
        # _lease and _connection are always set together in is_leader().
        lease = self._lease
        assert lease is not None
        try:
            if self._held:
                await self._connection.execute("SELECT pg_advisory_unlock($1)", self._lock_id)
        finally:
            await lease.__aexit__(None, None, None)
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
        max_offers_per_tick: int = 50,
        max_consecutive_failures: int = 3,
    ) -> None:
        if offer_ttl <= timedelta():
            raise ValueError("offer TTL must be positive")
        if max_offers_per_tick < 1:
            raise ValueError("max offers per tick must be positive")
        if max_consecutive_failures < 1:
            raise ValueError("max consecutive failures must be positive")
        self._control_plane = control_plane
        self._deliver_offer = deliver_offer
        self._task_packet = task_packet
        self._offer_ttl = offer_ttl
        self._max_offers_per_tick = max_offers_per_tick
        self._max_consecutive_failures = max_consecutive_failures

    async def tick(self, now: datetime) -> list[ScheduledJob]:
        """Place offers until no candidate remains or the per-tick budget is hit.

        A single fixed-interval pass previously placed at most one offer, so N
        queued jobs and N idle runners took N intervals to fully dispatch.

        A candidate that cannot be served is never fatal to the pass (issue
        #91): one unreachable runner must not stall every other ready job for a
        full interval, and neither failure mode may terminate a workflow.

        - A packet build error is an orchestrator-side fault. The offer is left
          alone so nothing about the run is decided here; the control plane
          releases it when the offer TTL elapses.
        - An undelivered offer (returned false or raised) is released back to
          the control plane, which requeues an in-flight run and only cancels a
          bootstrap run that holds no work.

        Consecutive failures are capped so a fleet-wide outage cannot churn a
        whole per-tick budget of offers every interval.
        """
        expire_offers = getattr(self._control_plane, "expire_offers", None)
        if expire_offers is not None:
            await _await(expire_offers(now))
        expire_leases = getattr(self._control_plane, "expire_leases", None)
        if expire_leases is not None:
            await _await(expire_leases(now))

        placed: list[ScheduledJob] = []
        consecutive_failures = 0
        for _ in range(self._max_offers_per_tick):
            scheduled = await self._schedule_one(now)
            if scheduled is None:
                break
            if not await self._place(scheduled, now):
                consecutive_failures += 1
                if consecutive_failures >= self._max_consecutive_failures:
                    break
                continue
            consecutive_failures = 0
            placed.append(scheduled)
        return placed

    async def _place(self, scheduled: ScheduledJob, now: datetime) -> bool:
        context = {"job_id": scheduled.offer.job_id, "runner_id": scheduled.offer.runner_id}
        try:
            task_packet = await _await(self._task_packet(scheduled))
        except Exception:
            _LOGGER.exception("task packet build failed; leaving the offer to expire", extra=context)
            return False
        delivery_error: Exception | None = None
        try:
            delivered = await self._deliver_offer(scheduled.offer, task_packet)
        except Exception as error:
            _LOGGER.exception("offer delivery failed; releasing the offer", extra=context)
            delivery_error = error
            delivered = False
        if delivered:
            return True
        await self._reject_offer(scheduled, now, delivery_error)
        return False

    async def _schedule_one(self, now: datetime) -> ScheduledJob | None:
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
        return scheduled

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
        on_tick: Callable[[], None] | None = None,
    ) -> None:
        if interval <= timedelta():
            raise ValueError("scheduler interval must be positive")
        while not stop_event.is_set():
            try:
                if await _await(is_leader()):
                    await self.tick(now())
                if on_tick is not None:
                    on_tick()
            except asyncio.CancelledError:
                raise
            except Exception:
                _LOGGER.exception("scheduler tick failed; retrying after backoff")
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
        on_tick: Callable[[], None] | None = None,
    ) -> None:
        try:
            await self.run(stop_event, now, interval, leader.is_leader, on_tick=on_tick)
        finally:
            await leader.close()
