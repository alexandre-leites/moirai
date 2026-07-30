import asyncio
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any

try:
    import grpc

    from moirai.domain.control_plane import InMemoryControlPlane
    from moirai.domain.models import Issue, Project
    from moirai.grpc.runner_control import RunnerControlService
    from proto import runner_control_pb2, runner_control_pb2_grpc
except ModuleNotFoundError:
    grpc = None


NOW = datetime(2026, 1, 1, tzinfo=UTC)


@unittest.skipIf(grpc is None, "grpcio is not installed")
class RunnerControlGrpcTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        self.server = grpc.aio.server()
        self.server_service = RunnerControlService(self.control_plane, now=lambda: NOW)
        runner_control_pb2_grpc.add_RunnerControlServicer_to_server(self.server_service, self.server)
        port = self.server.add_insecure_port("127.0.0.1:0")
        await self.server.start()
        self.channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        self.client = runner_control_pb2_grpc.RunnerControlStub(self.channel)

    async def asyncTearDown(self) -> None:
        await self.channel.close()
        await self.server.stop(0)

    async def _await_session(self, runner_id: str) -> None:
        for _ in range(200):
            if await self.server_service._sessions.connected(runner_id):
                return
            await asyncio.sleep(0.01)
        self.fail("runner session was never established")

    async def _await_draining(self, runner_id: str, expected: bool) -> None:
        for _ in range(200):
            _, runners, _ = self.control_plane.snapshot()
            if any(runner.id == runner_id and runner.draining is expected for runner in runners):
                return
            await asyncio.sleep(0.01)
        self.fail(f"runner draining flag never became {expected}")

    async def _connected_runner(self) -> tuple[Any, Any]:
        """Registers a runner with a live Connect stream and one eligible issue."""
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        registered = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=token, name="runner-a", labels=["docker"], protocol_version="1.0"
            )
        )
        self.control_plane.add_project(Project("project-a", True, frozenset({"docker"})))
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 1, NOW, NOW, True))
        stream = self.client.Connect()
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                heartbeat=runner_control_pb2.Heartbeat(labels=["docker"]),
            )
        )
        await self._await_session(registered.runner_id)
        return registered, stream

    async def test_register_runner_exchanges_a_scoped_token_for_a_credential(self) -> None:
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))

        response = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=token,
                name="runner-a",
                labels=["docker"],
                protocol_version="1.0",
            )
        )

        self.assertTrue(response.runner_id)
        self.assertTrue(response.credential)
        with self.assertRaises(grpc.aio.AioRpcError) as reused:
            await self.client.RegisterRunner(
                runner_control_pb2.RegisterRunnerRequest(
                    token=token,
                    name="runner-b",
                    labels=["docker"],
                    protocol_version="1.0",
                )
            )
        self.assertEqual(reused.exception.code(), grpc.StatusCode.PERMISSION_DENIED)
        self.assertNotIn(token, reused.exception.details())

    async def test_register_runner_rejects_invalid_protocol_and_input(self) -> None:
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        with self.assertRaises(grpc.aio.AioRpcError) as unsupported:
            await self.client.RegisterRunner(
                runner_control_pb2.RegisterRunnerRequest(
                    token=token,
                    name="runner-a",
                    labels=["docker"],
                    protocol_version="2.0",
                )
            )
        self.assertEqual(unsupported.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        with self.assertRaises(grpc.aio.AioRpcError) as invalid:
            await self.client.RegisterRunner(
                runner_control_pb2.RegisterRunnerRequest(
                    token=token,
                    name=" ",
                    labels=["docker"],
                    protocol_version="1.0",
                )
            )
        self.assertEqual(invalid.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_connect_heartbeat_authenticates_and_marks_runner_available(self) -> None:
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        registered = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=token,
                name="runner-a",
                labels=["docker"],
                protocol_version="1.0",
            )
        )

        stream = self.client.Connect()
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                heartbeat=runner_control_pb2.Heartbeat(labels=["docker"]),
            )
        )
        await stream.done_writing()
        self.assertEqual(await stream.read(), grpc.aio.EOF)

        _, runners, _ = self.control_plane.snapshot()
        self.assertEqual(len(runners), 1)
        self.assertTrue(runners[0].available)

    async def test_connect_rejects_invalid_runner_credentials(self) -> None:
        stream = self.client.Connect()
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id="unknown",
                credential="invalid",
                heartbeat=runner_control_pb2.Heartbeat(),
            )
        )
        await stream.done_writing()
        with self.assertRaises(grpc.aio.AioRpcError) as rejected:
            await stream.read()
        self.assertEqual(rejected.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_connect_accepts_offer_and_acknowledges_lease_renewal(self) -> None:
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        registered = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=token, name="runner-a", labels=["docker"], protocol_version="1.0"
            )
        )
        self.control_plane.add_project(Project("project-a", True, frozenset({"docker"})))
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 1, NOW, NOW, True))
        stream = self.client.Connect()
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                heartbeat=runner_control_pb2.Heartbeat(labels=["docker"]),
            )
        )
        for _ in range(20):
            if await self.server_service._sessions.connected(registered.runner_id):
                break
            await asyncio.sleep(0.01)
        self.assertTrue(await self.server_service._sessions.connected(registered.runner_id))
        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=1))
        assert scheduled is not None
        self.assertTrue(await self.server_service.deliver_offer(scheduled.offer, {"protocolVersion": "1.0"}))
        offer = await stream.read()
        self.assertEqual(offer.offer.job_id, scheduled.offer.job_id)
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                offer_accepted=runner_control_pb2.JobOfferAccepted(job_id=scheduled.offer.job_id),
            )
        )
        acceptance = await stream.read()
        self.assertEqual(acceptance.lease_acknowledged.job_id, scheduled.offer.job_id)
        renewed_expiry = NOW + timedelta(minutes=2)
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                lease_renewal=runner_control_pb2.LeaseRenewal(
                    job_id=scheduled.offer.job_id,
                    lease_generation=acceptance.lease_acknowledged.lease_generation,
                    requested_expires_at_unix_ms=int(renewed_expiry.timestamp() * 1000),
                ),
            )
        )
        renewal = await stream.read()
        self.assertEqual(renewal.lease_acknowledged.expires_at_unix_ms, int(renewed_expiry.timestamp() * 1000))
        await stream.done_writing()

    async def test_operator_drain_delivers_control_message_and_keeps_stream_open(self) -> None:
        registered, stream = await self._connected_runner()

        self.assertTrue(await self.server_service.set_draining(registered.runner_id, True))
        self.assertIsNotNone((await stream.read()).drain)
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                runner_draining=runner_control_pb2.RunnerDraining(draining=True),
            )
        )
        await self._await_draining(registered.runner_id, True)
        self.assertIsNone(self.control_plane.schedule(NOW, timedelta(minutes=1)))
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                heartbeat=runner_control_pb2.Heartbeat(labels=["docker"]),
            )
        )
        await stream.done_writing()
        self.assertEqual(await stream.read(), grpc.aio.EOF)

    async def test_connect_drain_report_keeps_the_stream_open_and_stops_scheduling(self) -> None:
        registered, stream = await self._connected_runner()

        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                runner_draining=runner_control_pb2.RunnerDraining(draining=True),
            )
        )
        await self._await_draining(registered.runner_id, True)

        self.assertIsNone(self.control_plane.schedule(NOW, timedelta(minutes=1)))
        # The stream survived the drain report. Before the handler existed the
        # message fell through to the catch-all and Connect aborted with
        # INVALID_ARGUMENT, so this write went nowhere and the read below raised
        # instead of reporting a clean half-close.
        await stream.write(
            runner_control_pb2.RunnerToOrchestrator(
                runner_id=registered.runner_id,
                credential=registered.credential,
                heartbeat=runner_control_pb2.Heartbeat(labels=["docker"]),
            )
        )
        await stream.done_writing()
        self.assertEqual(await stream.read(), grpc.aio.EOF)

    async def test_connect_drain_report_of_false_clears_the_flag(self) -> None:
        registered, stream = await self._connected_runner()

        for reported in (True, False):
            await stream.write(
                runner_control_pb2.RunnerToOrchestrator(
                    runner_id=registered.runner_id,
                    credential=registered.credential,
                    runner_draining=runner_control_pb2.RunnerDraining(draining=reported),
                )
            )
            await self._await_draining(registered.runner_id, reported)

        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=1))
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.runner.id, registered.runner_id)
        self.assertTrue(await self.server_service.deliver_offer(scheduled.offer, {"protocolVersion": "1.0"}))
        offer = await stream.read()
        self.assertEqual(offer.offer.job_id, scheduled.offer.job_id)
        await stream.done_writing()


if __name__ == "__main__":
    unittest.main()
