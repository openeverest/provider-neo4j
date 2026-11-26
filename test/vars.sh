#!/bin/bash

# Environment variables sourced by the `make test-integration*` targets before
# running chainsaw. Use it to pin the operator/engine versions your tests run
# against so they are reproducible locally and in CI.
#
# Reference values in chainsaw test files via ($values) bindings or plain
# environment substitution in `script:` steps.

export PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH:-${PWD}}
echo "PROVIDER_ROOT_PATH=${PROVIDER_ROOT_PATH}"

export NEO4J_OPERATOR_VERSION=${NEO4J_OPERATOR_VERSION:-"1.1.0"}
echo "NEO4J_OPERATOR_VERSION=${NEO4J_OPERATOR_VERSION}"

export NEO4J_VERSION=${NEO4J_VERSION:-"2026.10.2"}
echo "NEO4J_VERSION=${NEO4J_VERSION}"

# The released provider the e2e upgrade suite starts from.
export NEO4J_PROVIDER_PREVIOUS_RELEASE=${NEO4J_PROVIDER_PREVIOUS_RELEASE:-"0.1.1"}
echo "NEO4J_PROVIDER_PREVIOUS_RELEASE=${NEO4J_PROVIDER_PREVIOUS_RELEASE}"
