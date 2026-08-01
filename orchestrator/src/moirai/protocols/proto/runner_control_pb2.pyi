from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RegisterRunnerRequest(_message.Message):
    __slots__ = ("token", "name", "labels", "protocol_version", "capacity")
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    LABELS_FIELD_NUMBER: _ClassVar[int]
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    CAPACITY_FIELD_NUMBER: _ClassVar[int]
    token: str
    name: str
    labels: _containers.RepeatedScalarFieldContainer[str]
    protocol_version: str
    capacity: int
    def __init__(self, token: _Optional[str] = ..., name: _Optional[str] = ..., labels: _Optional[_Iterable[str]] = ..., protocol_version: _Optional[str] = ..., capacity: _Optional[int] = ...) -> None: ...

class RegisterRunnerResponse(_message.Message):
    __slots__ = ("runner_id", "credential")
    RUNNER_ID_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_FIELD_NUMBER: _ClassVar[int]
    runner_id: str
    credential: str
    def __init__(self, runner_id: _Optional[str] = ..., credential: _Optional[str] = ...) -> None: ...

class RunnerToOrchestrator(_message.Message):
    __slots__ = ("runner_id", "credential", "heartbeat", "event", "offer_accepted", "offer_rejected", "lease_renewal", "runner_draining")
    RUNNER_ID_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_FIELD_NUMBER: _ClassVar[int]
    EVENT_FIELD_NUMBER: _ClassVar[int]
    OFFER_ACCEPTED_FIELD_NUMBER: _ClassVar[int]
    OFFER_REJECTED_FIELD_NUMBER: _ClassVar[int]
    LEASE_RENEWAL_FIELD_NUMBER: _ClassVar[int]
    RUNNER_DRAINING_FIELD_NUMBER: _ClassVar[int]
    runner_id: str
    credential: str
    heartbeat: Heartbeat
    event: ExecutionEvent
    offer_accepted: JobOfferAccepted
    offer_rejected: JobOfferRejected
    lease_renewal: LeaseRenewal
    runner_draining: RunnerDraining
    def __init__(self, runner_id: _Optional[str] = ..., credential: _Optional[str] = ..., heartbeat: _Optional[_Union[Heartbeat, _Mapping]] = ..., event: _Optional[_Union[ExecutionEvent, _Mapping]] = ..., offer_accepted: _Optional[_Union[JobOfferAccepted, _Mapping]] = ..., offer_rejected: _Optional[_Union[JobOfferRejected, _Mapping]] = ..., lease_renewal: _Optional[_Union[LeaseRenewal, _Mapping]] = ..., runner_draining: _Optional[_Union[RunnerDraining, _Mapping]] = ...) -> None: ...

class OrchestratorToRunner(_message.Message):
    __slots__ = ("offer", "cancel", "lease_acknowledged", "drain")
    OFFER_FIELD_NUMBER: _ClassVar[int]
    CANCEL_FIELD_NUMBER: _ClassVar[int]
    LEASE_ACKNOWLEDGED_FIELD_NUMBER: _ClassVar[int]
    DRAIN_FIELD_NUMBER: _ClassVar[int]
    offer: JobOffer
    cancel: CancelExecution
    lease_acknowledged: LeaseAcknowledged
    drain: DrainRunner
    def __init__(self, offer: _Optional[_Union[JobOffer, _Mapping]] = ..., cancel: _Optional[_Union[CancelExecution, _Mapping]] = ..., lease_acknowledged: _Optional[_Union[LeaseAcknowledged, _Mapping]] = ..., drain: _Optional[_Union[DrainRunner, _Mapping]] = ...) -> None: ...

class Heartbeat(_message.Message):
    __slots__ = ("labels", "busy")
    LABELS_FIELD_NUMBER: _ClassVar[int]
    BUSY_FIELD_NUMBER: _ClassVar[int]
    labels: _containers.RepeatedScalarFieldContainer[str]
    busy: bool
    def __init__(self, labels: _Optional[_Iterable[str]] = ..., busy: _Optional[bool] = ...) -> None: ...

class JobOffer(_message.Message):
    __slots__ = ("job_id", "lease_generation", "task_packet_json")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    TASK_PACKET_JSON_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    lease_generation: int
    task_packet_json: str
    def __init__(self, job_id: _Optional[str] = ..., lease_generation: _Optional[int] = ..., task_packet_json: _Optional[str] = ...) -> None: ...

class JobOfferAccepted(_message.Message):
    __slots__ = ("job_id",)
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    def __init__(self, job_id: _Optional[str] = ...) -> None: ...

class JobOfferRejected(_message.Message):
    __slots__ = ("job_id", "reason")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    reason: str
    def __init__(self, job_id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class LeaseRenewal(_message.Message):
    __slots__ = ("job_id", "lease_generation", "requested_expires_at_unix_ms")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    REQUESTED_EXPIRES_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    lease_generation: int
    requested_expires_at_unix_ms: int
    def __init__(self, job_id: _Optional[str] = ..., lease_generation: _Optional[int] = ..., requested_expires_at_unix_ms: _Optional[int] = ...) -> None: ...

class LeaseAcknowledged(_message.Message):
    __slots__ = ("job_id", "lease_generation", "expires_at_unix_ms")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    lease_generation: int
    expires_at_unix_ms: int
    def __init__(self, job_id: _Optional[str] = ..., lease_generation: _Optional[int] = ..., expires_at_unix_ms: _Optional[int] = ...) -> None: ...

class CancelExecution(_message.Message):
    __slots__ = ("execution_id", "lease_generation")
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    execution_id: str
    lease_generation: int
    def __init__(self, execution_id: _Optional[str] = ..., lease_generation: _Optional[int] = ...) -> None: ...

class DrainRunner(_message.Message):
    __slots__ = ("undrain",)
    UNDRAIN_FIELD_NUMBER: _ClassVar[int]
    undrain: bool
    def __init__(self, undrain: _Optional[bool] = ...) -> None: ...

class RunnerDraining(_message.Message):
    __slots__ = ("draining",)
    DRAINING_FIELD_NUMBER: _ClassVar[int]
    draining: bool
    def __init__(self, draining: _Optional[bool] = ...) -> None: ...

class ExecutionEvent(_message.Message):
    __slots__ = ("job_id", "execution_id", "lease_generation", "event_sequence", "type", "payload_json")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    EVENT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    execution_id: str
    lease_generation: int
    event_sequence: int
    type: str
    payload_json: str
    def __init__(self, job_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., lease_generation: _Optional[int] = ..., event_sequence: _Optional[int] = ..., type: _Optional[str] = ..., payload_json: _Optional[str] = ...) -> None: ...

class ResolveJobSecretRequest(_message.Message):
    __slots__ = ("runner_id", "credential", "job_id", "lease_generation", "name")
    RUNNER_ID_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_FIELD_NUMBER: _ClassVar[int]
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    LEASE_GENERATION_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    runner_id: str
    credential: str
    job_id: str
    lease_generation: int
    name: str
    def __init__(self, runner_id: _Optional[str] = ..., credential: _Optional[str] = ..., job_id: _Optional[str] = ..., lease_generation: _Optional[int] = ..., name: _Optional[str] = ...) -> None: ...

class ResolveJobSecretResponse(_message.Message):
    __slots__ = ("value", "delivery")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    DELIVERY_FIELD_NUMBER: _ClassVar[int]
    value: str
    delivery: str
    def __init__(self, value: _Optional[str] = ..., delivery: _Optional[str] = ...) -> None: ...
