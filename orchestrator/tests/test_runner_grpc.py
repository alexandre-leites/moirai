import asyncio
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any

try:
    import grpc

    from moirai.domain.control_plane import InMemoryControlPlane
    from moirai.domain.models import Issue, Project
    from moirai.grpc.runner_control import RunnerControlService
    from moirai.persistence.secrets import SecretCipherError
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


@unittest.skipIf(grpc is None, "grpcio is not installed")
class ResolveJobSecretTests(unittest.IsolatedAsyncioTestCase):
    """The one RPC that sends credential material outward.

    Every test here is a refusal except the first: this is the boundary where a
    mistake hands a secret to something that should not have it, so what it
    declines matters more than what it returns.
    """

    async def asyncSetUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        await self._start(secure_channel=True)

    async def _start(self, secure_channel: bool) -> None:
        self.server = grpc.aio.server()
        self.server_service = RunnerControlService(
            self.control_plane, now=lambda: NOW, secure_channel=secure_channel
        )
        runner_control_pb2_grpc.add_RunnerControlServicer_to_server(self.server_service, self.server)
        port = self.server.add_insecure_port("127.0.0.1:0")
        await self.server.start()
        self.channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        self.client = runner_control_pb2_grpc.RunnerControlStub(self.channel)
        self.addAsyncCleanup(self.server.stop, 0)
        self.addAsyncCleanup(self.channel.close)

    async def _holding_runner(self) -> tuple[str, str, str, int]:
        """A registered runner holding an accepted job, with its lease generation."""
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        registered = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=token, name="runner-a", labels=["docker"], protocol_version="1.0"
            )
        )
        self.control_plane.add_project(Project("project-a", True, frozenset({"docker"})))
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 1, NOW, NOW, True))
        # Placement only considers a connected runner; the stream is what marks
        # one in production, and a heartbeat is the same thing without one.
        self.control_plane.heartbeat(registered.runner_id, registered.credential, NOW)
        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=1))
        assert scheduled is not None
        lease = self.control_plane.accept_offer(scheduled.offer.job_id, registered.runner_id, NOW)
        return registered.runner_id, registered.credential, scheduled.offer.job_id, lease.generation

    def _request(self, runner_id: str, credential: str, job_id: str, generation: int, name: str = "GITHUB_TOKEN") -> Any:
        return runner_control_pb2.ResolveJobSecretRequest(
            runner_id=runner_id, credential=credential, job_id=job_id,
            lease_generation=generation, name=name,
        )

    async def test_a_runner_holding_the_job_receives_the_projects_credential(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")

        response = await self.client.ResolveJobSecret(
            self._request(runner_id, credential, job_id, generation)
        )

        self.assertEqual(response.value, "ghp_project")
        self.assertEqual(response.delivery, "environment")

    async def test_an_ssh_key_is_delivered_as_a_file_not_a_variable(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "ssh_private_key", "-----BEGIN KEY-----")

        response = await self.client.ResolveJobSecret(
            self._request(runner_id, credential, job_id, generation, name="GIT_SSH_KEY")
        )

        self.assertEqual(response.delivery, "file")

    async def test_a_project_without_a_credential_is_not_found_so_the_runner_falls_back(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(self._request(runner_id, credential, job_id, generation))

        self.assertEqual(raised.exception.code(), grpc.StatusCode.NOT_FOUND)

    async def test_a_stale_lease_generation_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")
        # Recovery took the job away and moved the generation on. The runner
        # does not know that yet and keeps asking with the one it was given --
        # which is exactly the case this fence exists for.
        self.control_plane.expire_leases(NOW + timedelta(hours=1))

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(
                self._request(runner_id, credential, job_id, generation)
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    async def test_a_runner_that_does_not_hold_the_job_is_refused(self) -> None:
        _, _, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")
        other = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(minutes=1))
        stranger = await self.client.RegisterRunner(
            runner_control_pb2.RegisterRunnerRequest(
                token=other, name="runner-b", labels=["docker"], protocol_version="1.0"
            )
        )

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(
                self._request(stranger.runner_id, stranger.credential, job_id, generation)
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    async def test_an_expired_lease_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")
        # The service asks at NOW; the lease this server was handed expires a
        # minute later, so move the clock past it rather than the lease.
        self.server_service._now = lambda: NOW + timedelta(hours=1)

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(self._request(runner_id, credential, job_id, generation))

        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    async def test_a_wrong_credential_is_refused_before_any_lookup(self) -> None:
        runner_id, _, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(
                self._request(runner_id, "not-the-credential", job_id, generation)
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_an_unstored_provider_name_falls_back_to_the_runner(self) -> None:
        """Any provider name is resolvable; having none is NOT_FOUND, not a bug.

        Before agent credentials, a name outside the two git kinds was rejected
        outright, which is what made a deployment-wide OPENROUTER_API_KEY
        impossible: the runner could never be told "I have nothing, use yours".
        """
        runner_id, credential, job_id, generation = await self._holding_runner()

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(
                self._request(runner_id, credential, job_id, generation, name="OPENROUTER_API_KEY")
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.NOT_FOUND)

    async def test_a_name_that_cannot_name_a_credential_is_rejected(self) -> None:
        """HOME is not a credential: it is what the agent's home directory is.

        Serving it would let a project repoint the throwaway home the runner
        built, and lowercase names are not environment references at all.
        """
        runner_id, credential, job_id, generation = await self._holding_runner()

        for name in ("HOME", "PATH", "not-an-env-name"):
            with self.subTest(name=name):
                with self.assertRaises(grpc.aio.AioRpcError) as raised:
                    await self.client.ResolveJobSecret(
                        self._request(runner_id, credential, job_id, generation, name=name)
                    )

                self.assertEqual(raised.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_an_unopenable_credential_says_so_instead_of_reporting_INTERNAL(self) -> None:
        """A missing or wrong LOOP_SECRET_KEY must name itself.

        Folding this into the generic handler reported "secret could not be
        resolved", which is indistinguishable from a broken runner. On a live
        deployment that cost three failed planning attempts and an open circuit
        before anyone could tell which it was. The message describes deployment
        configuration, never a credential, so it is safe to pass to the runner.
        """
        runner_id, credential, job_id, generation = await self._holding_runner()

        async def unopenable(*_: object, **__: object) -> None:
            raise SecretCipherError(
                "no secret key is configured, so per-project credentials cannot be "
                "stored or read; set LOOP_SECRET_KEY"
            )

        self.control_plane.resolve_job_secret = unopenable  # type: ignore[assignment]

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(self._request(runner_id, credential, job_id, generation))

        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        self.assertIn("LOOP_SECRET_KEY", raised.exception.details())

    async def test_an_incomplete_request_is_rejected(self) -> None:
        runner_id, credential, job_id, _ = await self._holding_runner()

        for request in (
            self._request("", credential, job_id, 1),
            self._request(runner_id, "", job_id, 1),
            self._request(runner_id, credential, "", 1),
            self._request(runner_id, credential, job_id, 0),
            self._request(runner_id, credential, job_id, 1, name="  "),
        ):
            with self.assertRaises(grpc.aio.AioRpcError) as raised:
                await self.client.ResolveJobSecret(request)
            self.assertEqual(raised.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)


@unittest.skipIf(grpc is None, "grpcio is not installed")
class ResolveJobSecretWithoutTlsTests(ResolveJobSecretTests):
    """Without TLS the value would cross the network in the clear.

    Refusing keeps a non-TLS deployment exactly as it is today: the runner
    resolves from its own environment and simply has no per-project
    credentials. Inheriting the suite above is deliberate -- *every* call must
    be refused here, including the ones that succeed with TLS.
    """

    async def asyncSetUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        await self._start(secure_channel=False)

    async def _expect_refusal(self, request: Any) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(request)
        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        self.assertIn("insecure channel", raised.exception.details())

    async def test_a_runner_holding_the_job_receives_the_projects_credential(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_an_ssh_key_is_delivered_as_a_file_not_a_variable(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "ssh_private_key", "-----BEGIN KEY-----")
        await self._expect_refusal(
            self._request(runner_id, credential, job_id, generation, name="GIT_SSH_KEY")
        )

    async def test_a_project_without_a_credential_is_not_found_so_the_runner_falls_back(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        # Refused before the lookup, so it never becomes NOT_FOUND.
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_stale_lease_generation_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.expire_leases(NOW + timedelta(hours=1))
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_runner_that_does_not_hold_the_job_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_an_expired_lease_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_wrong_credential_is_refused_before_any_lookup(self) -> None:
        runner_id, _, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, "not-the-credential", job_id, generation))

    async def test_an_unstored_provider_name_falls_back_to_the_runner(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(
            self._request(runner_id, credential, job_id, generation, name="OPENROUTER_API_KEY")
        )

    async def test_a_name_that_cannot_name_a_credential_is_rejected(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        for name in ("HOME", "PATH", "not-an-env-name"):
            with self.subTest(name=name):
                await self._expect_refusal(
                    self._request(runner_id, credential, job_id, generation, name=name)
                )

    async def test_an_incomplete_request_is_rejected(self) -> None:
        # The TLS gate is checked before request validation, deliberately: a
        # deployment that cannot serve secrets at all should say so first.
        await self._expect_refusal(self._request("", "", "", 0))

    async def test_an_unopenable_credential_says_so_instead_of_reporting_INTERNAL(self) -> None:
        # Without TLS nothing is resolved at all, so the key never comes up.
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))


@unittest.skipIf(grpc is None, "grpcio is not installed")
class StoreJobSecretTests(ResolveJobSecretTests):
    """The write-back for a credential the harness rotated mid-job (issue #230).

    Inherits the setup rather than the assertions: the fence, the identity and
    the TLS gate are the same boundary as ResolveJobSecret, and the reason a
    rotated access token can be trusted is that it arrives across exactly that
    boundary. The inherited resolve tests still run here, which is the point --
    adding a write path must not have loosened the read one.
    """

    def _store(
        self,
        runner_id: str,
        credential: str,
        job_id: str,
        generation: int,
        name: str = "OPENCODE_AUTH",
        value: str = '{"access":"rotated"}',
    ) -> Any:
        return runner_control_pb2.StoreJobSecretRequest(
            runner_id=runner_id, credential=credential, job_id=job_id,
            lease_generation=generation, name=name, value=value,
        )

    async def test_a_rotated_credential_replaces_the_stored_one(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential(
            "project-a", "agent:OPENCODE_AUTH", '{"access":"original"}', ".config/auth.json"
        )

        response = await self.client.StoreJobSecret(
            self._store(runner_id, credential, job_id, generation)
        )

        self.assertTrue(response.stored)
        resolved = await self.client.ResolveJobSecret(
            runner_control_pb2.ResolveJobSecretRequest(
                runner_id=runner_id, credential=credential, job_id=job_id,
                lease_generation=generation, name="OPENCODE_AUTH",
            )
        )
        self.assertEqual(resolved.value, '{"access":"rotated"}')
        self.assertEqual(resolved.delivery, "file")

    async def test_a_runner_cannot_introduce_a_credential(self) -> None:
        """An update, never an insert.

        A runner may replace a credential the project already gave it and
        nothing else, so a compromised one cannot plant a name a later execution
        would then resolve and use.
        """
        runner_id, credential, job_id, generation = await self._holding_runner()

        response = await self.client.StoreJobSecret(
            self._store(runner_id, credential, job_id, generation, name="OPENROUTER_API_KEY")
        )

        self.assertFalse(response.stored)

    async def test_a_stale_lease_cannot_write_back(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential(
            "project-a", "agent:OPENCODE_AUTH", "original", ".config/auth.json"
        )

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.StoreJobSecret(
                self._store(runner_id, credential, job_id, generation + 1)
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    async def test_a_wrong_credential_is_refused(self) -> None:
        runner_id, _, job_id, generation = await self._holding_runner()

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.StoreJobSecret(
                self._store(runner_id, "not-the-credential", job_id, generation)
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_a_git_credential_is_not_the_runners_to_rewrite(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.StoreJobSecret(
                self._store(runner_id, credential, job_id, generation, name="GITHUB_TOKEN")
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_an_empty_value_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()

        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.StoreJobSecret(
                self._store(runner_id, credential, job_id, generation, value="")
            )

        self.assertEqual(raised.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)


@unittest.skipIf(grpc is None, "grpcio is not installed")
class StoreJobSecretWithoutTlsTests(StoreJobSecretTests):
    """A rotated token is exactly as sensitive as the one it replaces."""

    async def asyncSetUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        await self._start(secure_channel=False)

    async def _expect_refusal(self, request: Any) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.ResolveJobSecret(request)
        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        self.assertIn("insecure channel", raised.exception.details())

    async def _expect_store_refusal(self, request: Any) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as raised:
            await self.client.StoreJobSecret(request)
        self.assertEqual(raised.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        self.assertIn("insecure channel", raised.exception.details())

    async def test_a_runner_holding_the_job_receives_the_projects_credential(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "github_token", "ghp_project")
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_an_ssh_key_is_delivered_as_a_file_not_a_variable(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.set_project_credential("project-a", "ssh_private_key", "-----BEGIN KEY-----")
        await self._expect_refusal(
            self._request(runner_id, credential, job_id, generation, name="GIT_SSH_KEY")
        )

    async def test_a_project_without_a_credential_is_not_found_so_the_runner_falls_back(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_stale_lease_generation_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        self.control_plane.expire_leases(NOW + timedelta(hours=1))
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_runner_that_does_not_hold_the_job_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_an_expired_lease_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_wrong_credential_is_refused_before_any_lookup(self) -> None:
        runner_id, _, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, "not-the-credential", job_id, generation))

    async def test_an_unstored_provider_name_falls_back_to_the_runner(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(
            self._request(runner_id, credential, job_id, generation, name="OPENROUTER_API_KEY")
        )

    async def test_a_name_that_cannot_name_a_credential_is_rejected(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        for name in ("HOME", "PATH", "not-an-env-name"):
            with self.subTest(name=name):
                await self._expect_refusal(
                    self._request(runner_id, credential, job_id, generation, name=name)
                )

    async def test_an_incomplete_request_is_rejected(self) -> None:
        await self._expect_refusal(self._request("", "", "", 0))

    async def test_an_unopenable_credential_says_so_instead_of_reporting_INTERNAL(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_refusal(self._request(runner_id, credential, job_id, generation))

    async def test_a_rotated_credential_replaces_the_stored_one(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(self._store(runner_id, credential, job_id, generation))

    async def test_a_runner_cannot_introduce_a_credential(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(
            self._store(runner_id, credential, job_id, generation, name="OPENROUTER_API_KEY")
        )

    async def test_a_stale_lease_cannot_write_back(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(self._store(runner_id, credential, job_id, generation + 1))

    async def test_a_wrong_credential_is_refused(self) -> None:
        runner_id, _, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(
            self._store(runner_id, "not-the-credential", job_id, generation)
        )

    async def test_a_git_credential_is_not_the_runners_to_rewrite(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(
            self._store(runner_id, credential, job_id, generation, name="GITHUB_TOKEN")
        )

    async def test_an_empty_value_is_refused(self) -> None:
        runner_id, credential, job_id, generation = await self._holding_runner()
        await self._expect_store_refusal(
            self._store(runner_id, credential, job_id, generation, value="")
        )
