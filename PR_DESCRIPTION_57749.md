# PR Description — Issue #57749: MSSQL RPC failure under Istio TLS origination

## Summary
This PR addresses a compatibility issue where SQL text queries succeed but stored-procedure calls (TDS RPC) fail when traffic to Microsoft SQL Server is routed through Istio TLS origination and egress gateway paths.

Closes #57749.

## Problem Statement
In environments with:
- SQL Server forced encryption enabled (self-signed cert),
- headless service + service entry routing through egress gateway,
- destination rule TLS origination (mode `SIMPLE`),

applications using modern Microsoft SQL clients (for example `Microsoft.Data.SqlClient` / EF) can fail on stored-procedure invocation with:

`The incoming tabular data stream (TDS) remote procedure call (RPC) protocol stream is incorrect. Parameter 1: The parameter name is invalid`

Plain command text queries succeed, and stored procedures may still work from CLI tooling, indicating a protocol-path-specific behavior difference for RPC framing.

## Root Cause Hypothesis
Likely interaction between:
1. TDS RPC packet semantics from modern client drivers, and
2. TLS origination / protocol handling in the egress path,

where payload framing, segmentation, or interpretation differs from simple text-query flows.

## Proposed Changes
1. Reproduce with deterministic integration scenario:
   - SQL Server 2022 with forced encryption and representative stored procedure.
   - Client coverage for both command text and RPC invocation.
2. Add focused diagnostics in egress/origination path to compare:
   - successful text-query session behavior,
   - failing RPC session behavior.
3. Implement protocol-safe handling adjustments in the affected path.
4. Add regression tests to verify:
   - stored procedure RPC succeeds under TLS origination,
   - existing text-query behavior remains unchanged.

## Scope
- In scope: behavior specific to MSSQL TDS RPC over Istio-managed TLS origination flows.
- Out of scope: generalized SQL client troubleshooting unrelated to Istio egress path.

## Risk Assessment
- **Risk**: transport-path changes could impact non-MSSQL TCP/TLS egress behaviors.
- **Mitigation**:
  - keep fix narrowly scoped to the identified handling path,
  - add regression tests for existing egress TCP/TLS behaviors,
  - validate with both SQL text and RPC workloads.

## Validation Plan
- Automated:
  - add/extend integration tests for MSSQL text query + stored procedure RPC through egress TLS origination.
  - run existing relevant egress integration suites for regressions.
- Manual:
  1. Deploy reproduction topology from issue.
  2. Execute text query from app and verify success.
  3. Execute stored procedure via `Microsoft.Data.SqlClient` and verify success.
  4. Confirm no regression in baseline non-RPC SQL traffic.

## Backward Compatibility
- No API changes expected.
- Behavioral change limited to fixing incorrect failure in specific MSSQL RPC flow.

## Rollout
- Merge with tests and release-note mention under networking/egress compatibility.
- Recommend affected users re-test stored-procedure workloads after upgrade.

## Checklist
- [x] Full local PR description drafted.
- [ ] Functional code changes added.
- [ ] Regression tests added and passing.
