import unittest

from moirai.grpc.sessions import RunnerSessionRegistry


class RunnerSessionRegistryTests(unittest.IsolatedAsyncioTestCase):
    async def test_delivers_one_offer_to_a_connected_runner(self) -> None:
        registry = RunnerSessionRegistry()
        session = await registry.connect("runner-1")

        self.assertTrue(await registry.deliver_offer("runner-1", "job-1", {"job": "job-1"}))
        self.assertEqual(await session.next_message(), {"job": "job-1"})
        self.assertFalse(await registry.deliver_offer("runner-1", "job-2", {"job": "job-2"}))
        self.assertTrue(await registry.clear_offer("runner-1", "job-1"))
        self.assertTrue(await registry.deliver_offer("runner-1", "job-2", {"job": "job-2"}))

    async def test_does_not_deliver_to_a_disconnected_runner(self) -> None:
        registry = RunnerSessionRegistry()
        session = await registry.connect("runner-1")
        await registry.disconnect(session)

        self.assertFalse(await registry.connected("runner-1"))
        self.assertFalse(await registry.deliver_offer("runner-1", "job-1", {"job": "job-1"}))
        self.assertIsNone(await session.next_message())

    async def test_replacement_connection_closes_the_previous_session(self) -> None:
        registry = RunnerSessionRegistry()
        previous = await registry.connect("runner-1")
        current = await registry.connect("runner-1")

        self.assertIsNone(await previous.next_message())
        self.assertTrue(await registry.deliver_offer("runner-1", "job-1", {"job": "job-1"}))
        self.assertEqual(await current.next_message(), {"job": "job-1"})
        await registry.disconnect(previous)
        self.assertTrue(await registry.connected("runner-1"))


if __name__ == "__main__":
    unittest.main()
