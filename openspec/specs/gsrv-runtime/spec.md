# gsrv-runtime Specification

## Purpose
TBD - created by archiving change extract-gsrv-core-and-service-template. Update Purpose after archive.
## Requirements
### Requirement: Public runtime owns bounded lifecycle
The runtime SHALL bind enabled listeners before readiness, unwind startup failures, drain on cancellation or server failure, and close owned resources. It MUST reject repeated Run calls.
#### Scenario: Second listener fails
- **WHEN** a later listener cannot bind
- **THEN** earlier listeners and acquired resources are closed and readiness remains false
#### Scenario: Process stops
- **WHEN** the parent context is canceled
- **THEN** readiness becomes false before bounded shutdown and all server goroutines are joined

### Requirement: Shared readiness and strict configuration
The runtime SHALL expose HTTP liveness/readiness and gRPC Health with shared readiness, bounded critical checks, and strict single-document YAML.
#### Scenario: Dependency fails
- **WHEN** a critical check fails
- **THEN** readiness is unavailable while liveness remains healthy
#### Scenario: Invalid YAML
- **WHEN** configuration contains unknown keys or multiple documents
- **THEN** configuration loading fails before service startup

