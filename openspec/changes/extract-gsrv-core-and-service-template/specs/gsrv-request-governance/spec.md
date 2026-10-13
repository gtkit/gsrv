## ADDED Requirements

### Requirement: Authenticate caller identity
The public HTTP adapter SHALL authenticate bounded raw requests using HMAC-SHA256, timestamp and atomic nonce consumption. The public gRPC adapter SHALL bind tokens to AppKeys and reject absent authentication providers for business calls.
#### Scenario: Replay or tampering
- **WHEN** a signed request is repeated or method, URI, content type or body changes
- **THEN** authentication fails before application execution
#### Scenario: Forged caller identity
- **WHEN** a token is submitted with another AppKey
- **THEN** authentication fails

### Requirement: Share quota and final access record
HTTP and gRPC SHALL consume the same per-authenticated-AppKey limiter and produce final request logs with duration, verified AppKey and protocol/error codes.
#### Scenario: Mixed protocol quota
- **WHEN** one protocol has consumed the caller quota
- **THEN** the other protocol rejects that caller without affecting another AppKey
#### Scenario: Panic and rejected request
- **WHEN** processing panics or authentication rejects a request
- **THEN** a sanitized final access record is emitted

### Requirement: Business supplied resilience
The framework SHALL provide a breaker wrapper and optional bounded fallback while leaving business rules to the consumer.
#### Scenario: No fallback
- **WHEN** an operation fails and no fallback was provided
- **THEN** the original error is returned
#### Scenario: Caller cancellation
- **WHEN** the caller cancels or exhausts its deadline
- **THEN** no business fallback or retry is automatically performed

