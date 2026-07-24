from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Callable
from datetime import UTC, datetime
import inspect
import json
from typing import Any

import grpc

from moirai.domain.control_plane import AuthenticationError, JobOffer, OfferError, RegistrationError
from moirai.domain.leases import StaleLeaseError
from moirai.domain.models import ExecutionEvent
from moirai.grpc.sessions import RunnerSession, RunnerSessionRegistry
from moirai.workflows.runner_events import RunnerEventError, validate_runner_event
from proto import runner_control_pb2, runner_control_pb2_grpc


async def _await_if_needed(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class _StreamFailure(Exception):
    def __init__(self, code: grpc.StatusCode, detail: str) -> None:
        self.code = code
        self.detail = detail


class RunnerControlService(runner_control_pb2_grpc.RunnerControlServicer):
    def __init__(
        self,
        control_plane: Any,
        now: Callable[[], datetime] | None = None,
        sessions: RunnerSessionRegistry | None = None,
        workflow_runtime: Any | None = None,
    ) -> None:
        self._control_plane = control_plane
        self._now = now or (lambda: datetime.now(UTC))
        self._sessions = sessions or RunnerSessionRegistry()
        self._workflow_runtime = workflow_runtime

    async def RegisterRunner(
        self,
        request: runner_control_pb2.RegisterRunnerRequest,
        context: grpc.aio.ServicerContext,
    ) -> runner_control_pb2.RegisterRunnerResponse:
        if request.protocol_version != "1.0":
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                "runner protocol version is unsupported",
            )
        if not request.name.strip() or any(not label.strip() for label in request.labels):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "runner registration request is invalid")
        try:
            runner, credential = await _await_if_needed(
                self._control_plane.register_runner(
                    request.token,
                    request.name.strip(),
                    (label.strip() for label in request.labels),
                    self._now(),
                )
            )
        except RegistrationError:
            await context.abort(grpc.StatusCode.PERMISSION_DENIED, "runner registration was rejected")
        return runner_control_pb2.RegisterRunnerResponse(runner_id=runner.id, credential=credential)

    async def deliver_offer(self, offer: JobOffer, task_packet: dict[str, Any]) -> bool:
        if not isinstance(task_packet, dict):
            raise ValueError("task packet must be an object")
        message = runner_control_pb2.OrchestratorToRunner(
            offer=runner_control_pb2.JobOffer(
                job_id=offer.job_id,
                lease_generation=offer.lease.generation,
                task_packet_json=json.dumps(task_packet, separators=(",", ":"), sort_keys=True),
            )
        )
        return await self._sessions.deliver_offer(offer.runner_id, offer.job_id, message)

    async def clear_delivered_offer(self, runner_id: str, job_id: str) -> bool:
        return await self._sessions.clear_offer(runner_id, job_id)

    async def Connect(
        self,
        request_iterator: AsyncIterator[runner_control_pb2.RunnerToOrchestrator],
        context: grpc.aio.ServicerContext,
    ) -> AsyncIterator[runner_control_pb2.OrchestratorToRunner]:
        responses: asyncio.Queue[runner_control_pb2.OrchestratorToRunner | _StreamFailure | None] = asyncio.Queue()
        session: RunnerSession | None = None
        session_ready = asyncio.Event()

        async def receive() -> None:
            nonlocal session
            authenticated_runner_id: str | None = None
            try:
                async for request in request_iterator:
                    if not request.runner_id or not request.credential:
                        raise _StreamFailure(grpc.StatusCode.UNAUTHENTICATED, "runner authentication is required")
                    if authenticated_runner_id is not None and request.runner_id != authenticated_runner_id:
                        raise _StreamFailure(grpc.StatusCode.UNAUTHENTICATED, "runner identity cannot change")
                    try:
                        await _await_if_needed(
                            self._control_plane.authenticate_runner(
                                request.runner_id, request.credential, self._now()
                            )
                        )
                    except AuthenticationError as error:
                        raise _StreamFailure(
                            grpc.StatusCode.UNAUTHENTICATED, "runner authentication was rejected"
                        ) from error
                    if authenticated_runner_id is None:
                        authenticated_runner_id = str(request.runner_id)
                        session = await self._sessions.connect(authenticated_runner_id)
                        session_ready.set()
                    await self._handle_message(request, authenticated_runner_id)
            except _StreamFailure as error:
                await responses.put(error)
            finally:
                if session is not None:
                    await self._sessions.disconnect(session)
                await responses.put(None)

        receive_task = asyncio.create_task(receive())
        try:
            while True:
                if session is None:
                    session_task = asyncio.create_task(session_ready.wait())
                    response_task = asyncio.create_task(responses.get())
                    done, pending = await asyncio.wait(
                        {session_task, response_task}, return_when=asyncio.FIRST_COMPLETED
                    )
                    for pending_task in pending:
                        pending_task.cancel()
                    for pending_task in pending:
                        await _ignore_cancelled(pending_task)
                    if session_task in done:
                        continue
                    message = response_task.result()
                else:
                    session_message = asyncio.create_task(session.next_message())
                    response_message = asyncio.create_task(responses.get())
                    done, pending = await asyncio.wait(
                        {session_message, response_message}, return_when=asyncio.FIRST_COMPLETED
                    )
                    for pending_task in pending:
                        pending_task.cancel()
                    for pending_task in pending:
                        await _ignore_cancelled(pending_task)
                    message = response_message.result() if response_message in done else session_message.result()
                if message is None:
                    return
                if isinstance(message, _StreamFailure):
                    await context.abort(message.code, message.detail)
                yield message
        finally:
            if not receive_task.done():
                receive_task.cancel()
            await _ignore_cancelled(receive_task)
            if session is not None:
                await self._sessions.disconnect(session)

    async def _handle_message(
        self,
        request: runner_control_pb2.RunnerToOrchestrator,
        runner_id: str,
    ) -> None:
        message_type = request.WhichOneof("message")
        if message_type == "heartbeat":
            heartbeat = request.heartbeat
            if any(not label.strip() for label in heartbeat.labels):
                raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner heartbeat labels are invalid")
            await _await_if_needed(self._control_plane.heartbeat(runner_id, request.credential, self._now()))
            return
        if message_type == "offer_accepted":
            job_id = request.offer_accepted.job_id
            if not job_id:
                raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner offer acceptance is invalid")
            try:
                lease = await _await_if_needed(
                    self._control_plane.accept_offer(job_id, runner_id, self._now())
                )
            except OfferError as error:
                raise _StreamFailure(
                    grpc.StatusCode.FAILED_PRECONDITION, "runner offer acceptance was rejected"
                ) from error
            await self._sessions.clear_offer(runner_id, job_id)
            await self._send_lease_acknowledgement(runner_id, lease)
            return
        if message_type == "offer_rejected":
            job_id = request.offer_rejected.job_id
            if not job_id or len(request.offer_rejected.reason) > 1024:
                raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner offer rejection is invalid")
            try:
                await _await_if_needed(self._control_plane.reject_offer(job_id, runner_id, self._now()))
            except OfferError as error:
                raise _StreamFailure(
                    grpc.StatusCode.FAILED_PRECONDITION, "runner offer rejection was rejected"
                ) from error
            await self._sessions.clear_offer(runner_id, job_id)
            return
        if message_type == "lease_renewal":
            renewal = request.lease_renewal
            if not renewal.job_id or renewal.lease_generation < 1 or renewal.requested_expires_at_unix_ms <= 0:
                raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner lease renewal is invalid")
            try:
                expires_at = datetime.fromtimestamp(
                    renewal.requested_expires_at_unix_ms / 1000, tz=UTC
                )
                lease = await _await_if_needed(
                    self._control_plane.renew_lease(
                        renewal.job_id, runner_id, renewal.lease_generation, expires_at, self._now()
                    )
                )
            except StaleLeaseError as error:
                raise _StreamFailure(
                    grpc.StatusCode.FAILED_PRECONDITION, "runner lease renewal was rejected"
                ) from error
            except (OverflowError, OSError, ValueError) as error:
                raise _StreamFailure(
                    grpc.StatusCode.INVALID_ARGUMENT, "runner lease renewal is invalid"
                ) from error
            await self._send_lease_acknowledgement(runner_id, lease)
            return
        if message_type != "event":
            raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner stream message is invalid")
        event = request.event
        if (
            not event.job_id
            or not event.execution_id
            or not event.type.strip()
            or event.lease_generation < 1
            or event.event_sequence < 1
        ):
            raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner execution event is invalid")
        try:
            payload = json.loads(event.payload_json) if event.payload_json else {}
        except json.JSONDecodeError as error:
            raise _StreamFailure(
                grpc.StatusCode.INVALID_ARGUMENT, "runner execution event payload is invalid"
            ) from error
        if not isinstance(payload, dict):
            raise _StreamFailure(grpc.StatusCode.INVALID_ARGUMENT, "runner execution event payload is invalid")
        try:
            validate_runner_event(event.type.strip(), event.execution_id, payload)
        except RunnerEventError as error:
            raise _StreamFailure(
                grpc.StatusCode.INVALID_ARGUMENT, "runner execution event is invalid"
            ) from error
        try:
            accept_kwargs: dict[str, Any] = {"now": self._now()}
            if self._workflow_runtime is not None:
                accept_kwargs["on_transition"] = self._advance_workflow
            await _await_if_needed(
                self._control_plane.accept_event(
                    ExecutionEvent(
                        job_id=event.job_id,
                        runner_id=runner_id,
                        lease_generation=event.lease_generation,
                        event_sequence=event.event_sequence,
                        event_type=event.type.strip(),
                        execution_id=event.execution_id,
                        payload=payload,
                    ),
                    **accept_kwargs,
                )
            )
        except StaleLeaseError as error:
            raise _StreamFailure(
                grpc.StatusCode.FAILED_PRECONDITION, "runner execution event was rejected"
            ) from error

    async def _advance_workflow(self, workflow_run_id: str, new_status: str, state_updates: dict[str, object]) -> None:
        try:
            await self._workflow_runtime.run(workflow_run_id, state_updates)
        except Exception:
            import logging
            logging.getLogger("runner_control").exception(
                "workflow advancement after runner event failed"
            )

    async def _send_lease_acknowledgement(self, runner_id: str, lease: Any) -> None:
        expires_at = lease.expires_at
        if expires_at.tzinfo is None:
            raise ValueError("lease expiry must be timezone-aware")
        message = runner_control_pb2.OrchestratorToRunner(
            lease_acknowledged=runner_control_pb2.LeaseAcknowledged(
                job_id=lease.job_id,
                lease_generation=lease.generation,
                expires_at_unix_ms=int(expires_at.timestamp() * 1000),
            )
        )
        await self._sessions.deliver_message(runner_id, message)


async def _ignore_cancelled(task: asyncio.Task[Any]) -> None:
    try:
        await task
    except asyncio.CancelledError:
        pass
