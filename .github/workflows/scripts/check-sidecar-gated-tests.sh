#!/usr/bin/env bash
# Fails the caller if any of the sidecar's SIDECAR_MODEL_PATH-gated tests
# were skipped instead of actually executed.
#
# Why: these tests t.Skip silently when SIDECAR_MODEL_PATH (or, for a
# couple of them, SIDECAR_ONNX_LIB_DIR / a database / SIDECAR_SWEEP) is
# unset, and `go test` prints "ok" for a run that executed none of them --
# a green job that proves nothing.
#
# Usage: check-sidecar-gated-tests.sh <path to `go test -v` output>
#
# The list below is deliberately not "every test in the package" and not
# "zero SKIP lines allowed": several tests in these same packages skip on
# purpose even with SIDECAR_MODEL_PATH and SIDECAR_ONNX_LIB_DIR set:
#
#   - TestEmbedAppliesDistinctPromptPrefixesForNomic and
#     TestEmbedDocumentsAndEmbedQueryAgreeWithoutPrefixes gate on
#     SIDECAR_NOMIC_MODEL_PATH / SIDECAR_NOPREFIX_MODEL_PATH, separate model
#     checkouts this workflow does not fetch.
#   - TestEvalRecall and TestChunkingSweep gate on
#     SIDECAR_TEST_DATABASE_URL (TestChunkingSweep additionally on
#     SIDECAR_SWEEP=1); this job runs with no database, so these are
#     expected to skip here.
#
# So this script asserts a specific, named list of tests -- the ones
# gated on SIDECAR_MODEL_PATH (and, where used, SIDECAR_ONNX_LIB_DIR)
# alone -- actually reported PASS, not SKIP and not merely absent.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <go-test-v-output-file>" >&2
  exit 2
fi

log="$1"

if [ ! -s "$log" ]; then
  echo "ERROR: $log is missing or empty -- go test produced no output to check" >&2
  exit 1
fi

# internal/embed/onnx: gate on SIDECAR_MODEL_PATH only (see testEmbedder in
# onnx_test.go), plus TestBackendIsORT, which is unconditional.
# cmd/sidecar: TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit gates
# on SIDECAR_MODEL_PATH only -- it needs the tokenizer, not the database.
required_pass_tests=(
  "TestBackendIsORT"
  "TestEmbedReturns768Dimensions"
  "TestEmbedSeparatesParaphraseFromUnrelated"
  "TestEmbedRejectsEmptyBatch"
  "TestSplitPassageAtCapNeverExceedsNomicsRealTokenLimit"
)

fail=0
for name in "${required_pass_tests[@]}"; do
  if grep -qE -- "^--- SKIP: ${name}( |\$)" "$log"; then
    echo "ERROR: ${name} was SKIPPED instead of running." >&2
    echo "       SIDECAR_MODEL_PATH and/or SIDECAR_ONNX_LIB_DIR are not taking effect for this run." >&2
    fail=1
  elif ! grep -qE -- "^--- PASS: ${name}( |\$)" "$log"; then
    echo "ERROR: ${name} did not report PASS anywhere in the test output (missing, built with the wrong tag, or FAIL)." >&2
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "One or more gated model tests did not actually run. A skipped test must not read as a pass --" >&2
  echo "see this script's own header comment for the false-green incident this check exists to catch." >&2
  exit 1
fi

echo "OK: all ${#required_pass_tests[@]} SIDECAR_MODEL_PATH-gated tests executed and passed (none skipped)."
