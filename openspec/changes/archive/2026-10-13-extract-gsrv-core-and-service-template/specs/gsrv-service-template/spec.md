## ADDED Requirements

### Requirement: Independent service template
A template SHALL compile as an independent Go module using only public gsrv packages, include Linux deployment examples and configuration documentation, and support HTTP/gRPC registration without mandatory Redis or database.
#### Scenario: Fresh template build
- **WHEN** the template is copied to a separate directory and its gsrv replace is pointed to this checkout
- **THEN** it builds and serves health checks and its declared sample contract

### Requirement: Minimal validation service
The repository SHALL remove OCPC business code and dedicated dependencies and use a small InfoService in the original root service layers and the independent template to validate the public skeleton.
#### Scenario: Read service information
- **WHEN** an authenticated caller invokes GetInfo over gRPC or its signed HTTP route
- **THEN** it receives the service name and request ID without requiring a database, Redis or media SDK

### Requirement: Deliver root architecture documentation
The repository SHALL contain a root document covering architecture, request sequences, assembly, lifecycle, configurations and verified implementation stages.
#### Scenario: Developer starts another service
- **WHEN** a developer reads GSRV_DESIGN.md
- **THEN** public responsibilities, service ownership and template startup instructions are identifiable


### Requirement: Preserve original directory layering
Services SHALL retain cmd/server, internal/bootstrap, internal/platform/config, internal/runtime/module and internal/module application/transport layers. Generic implementations SHALL reside in internal/pkg, with interceptor and requestid remaining there. Tests SHALL be colocated with the implementation they exercise.
#### Scenario: Replace the validation module
- **WHEN** OCPC is replaced by InfoService
- **THEN** InfoService remains in the original module/runtime/transport layers and no new top-level capability directories replace internal/pkg
