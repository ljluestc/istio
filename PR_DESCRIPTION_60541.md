# issue #60541: gatewayapi ReplaceFullPath with RegularExpression should support capture substitution
## Summary
This change fixes Gateway API `HTTPRoute` URL rewrite behavior when `path.type` is `RegularExpression` and `replaceFullPath` uses capture references (for example `$1`). Istio now preserves the route's regex match for rewrite matching and normalizes `$N` references into Envoy-compatible `\N` substitutions.

## Problem
Reported behavior:
- `HTTPRoute` uses `matches.path.type: RegularExpression` with a capture group, such as `(?i)/(.*)`.
- `filters.urlRewrite.path.type: ReplaceFullPath` is set to `/api/$1`.
- The request path is rewritten to the literal string `/api/$1` instead of substituting the captured value.

## Root cause
Gateway API conversion used a fixed rewrite matcher (`/.*`) for `ReplaceFullPath`, so capture groups from the route's regex path were not available to the rewrite engine.

Additionally, user-facing `$N` capture notation was not normalized to Envoy regex substitution syntax (`\N`), so even when captures were conceptually intended, Envoy did not interpret `$1` as a capture backreference.

## What this PR changes
1. **Preserve route regex for full-path rewrites**
   - File: `pilot/pkg/config/kube/gateway/conversion.go`
   - `createRewriteFilter` now accepts route matches and, for a single regex path match, uses that regex as `UriRegexRewrite.Match` instead of the fixed `/.*`.
   - If route matching is ambiguous (for example multiple matches), behavior safely falls back to `/.*`.

2. **Normalize capture syntax for Envoy substitution**
   - File: `pilot/pkg/config/kube/gateway/conversion.go`
   - Added normalization that converts `$1`, `$2`, ... into `\1`, `\2`, ... for `UriRegexRewrite.Rewrite`.

3. **Regression tests**
   - File: `pilot/pkg/config/kube/gateway/conversion_test.go`
   - Added tests covering:
     - regex-path + full-path rewrite using `$1` (ensures regex is preserved and rewrite becomes `\1`)
     - fallback behavior when multiple matches exist (keeps `/.*` and still normalizes substitution)

## Validation
- `go test ./pilot/pkg/config/kube/gateway -run TestCreateRewriteFilter -count=1`
- `go test ./pilot/pkg/config/kube/gateway -count=1`

## Risk
Low to moderate.
- Scope is limited to Gateway API HTTPRoute conversion for `URLRewrite` + `ReplaceFullPath`.
- Includes explicit unit coverage for new behavior and fallback path.
- Does not affect unrelated VirtualService translation paths.

## Branch / commit
- Branch: `private/issue-60541-regex-rewrite-capture`
- Commit: _pending_

Co-Authored-By: Oz <oz-agent@warp.dev>
